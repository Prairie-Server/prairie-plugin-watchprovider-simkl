package provider

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
	"google.golang.org/protobuf/proto"
)

// Paging. Simkl answers each list in one response with no upstream paging,
// and a large library's list runs to thousands of rows, more than one page.
// Re-reading the list for every page would multiply calls against Simkl's
// daily quota, so a page token carries the rest of the list instead: each list
// is read once, sorted by a stable key, and handed out page by page from the
// token. The carried rows are bounded by carryBudgetBytes. A list too large to
// carry whole is re-read once per carried window and resumed after the last
// row returned, so a list costs one read per window instead of one per page.
//
// Resuming by key is safe for watched and progress reads: they are deltas, and
// a row that changes during the traversal moves the list's activity past the
// stamp the cursor records, so the next traversal reads it again. A snapshot
// (the watchlist, the dropped lists, or a complete ratings read) must not
// miss a row, so a re-read of a snapshot list must match the first read
// exactly, or the traversal fails as temporary and the next sync starts over.
const (
	defaultPageSize = 100
	maxPageSize     = 100
	// carryBudgetBytes bounds the encoded rows a page token carries before
	// compression. Compressed, a full budget is roughly 100 KiB.
	carryBudgetBytes = 512 << 10
	// maxTokenBytes bounds a decompressed page token.
	maxTokenBytes = 16 << 20

	listChangedMessage = "Simkl changed a list during the sync; the next sync starts over"
)

// pageToken is the state of a traversal between pages.
type pageToken struct {
	Kind int32 `json:"kind"`
	// Cursor is the cursor the traversal started from; every page must
	// repeat it.
	Cursor string `json:"cursor"`
	// Next is the cursor state the final page reports.
	Next     map[string]string `json:"next,omitempty"`
	Complete bool              `json:"complete,omitempty"`
	// Steps are the reads left. Steps[0] is in progress once Started.
	Steps   []step `json:"steps,omitempty"`
	Started bool   `json:"started,omitempty"`
	// More reports rows of Steps[0] beyond the carried ones, which a re-read
	// returns.
	More bool `json:"more,omitempty"`
	// After and AfterCount name the last row of Steps[0] handed out: its sort
	// key, and how many rows with that key were handed out.
	After      string `json:"after,omitempty"`
	AfterCount int    `json:"after_count,omitempty"`
	// Fingerprint identifies the first read of Steps[0], to detect a changed
	// snapshot list on a re-read.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Warnings counts each import warning of the traversal so far. The final
	// page returns them.
	Warnings map[string]int `json:"warnings,omitempty"`

	carried []*pluginv1.WatchSyncRemoteState
}

func (s *Server) listState(ctx context.Context, acct account, kind pluginv1.WatchSyncRemoteStateKind, req *pluginv1.WatchSyncListRemoteStateRequest) *pluginv1.WatchSyncListRemoteStateResponse {
	var (
		token *pageToken
		fault *pluginv1.WatchSyncFault
	)
	if strings.TrimSpace(req.GetPageToken()) == "" {
		token, fault = s.planTraversal(ctx, acct, kind, req.GetCursor())
	} else {
		token, fault = decodePageToken(req.GetPageToken(), kind, req.GetCursor())
	}
	if fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}
	}
	size := pageSize(req.GetPageSize())
	items := make([]*pluginv1.WatchSyncRemoteState, 0, size)
	// A page makes at most one list read, which keeps every call well inside
	// the host's deadline. A page may therefore return fewer rows than asked.
	read := false
	for len(items) < size {
		if len(token.carried) > 0 {
			n := min(size-len(items), len(token.carried))
			for _, row := range token.carried[:n] {
				token.handedOut(rowSortKey(row))
			}
			items = append(items, token.carried[:n]...)
			token.carried = token.carried[n:]
			continue
		}
		if token.Started && !token.More {
			token.finishStep()
			continue
		}
		if len(token.Steps) == 0 || read {
			break
		}
		read = true
		if fault := s.loadStep(ctx, acct, token, size-len(items)); fault != nil {
			return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}
		}
	}
	if token.Started && !token.More && len(token.carried) == 0 {
		token.finishStep()
	}
	response := &pluginv1.WatchSyncListRemoteStateResponse{Items: items, CompleteSnapshot: token.Complete}
	if len(token.Steps) == 0 {
		response.NextCursor = encodeCursor(token.Next)
		response.Warnings = token.warningMessages()
		return response
	}
	encoded, err := token.encode()
	if err != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: permanentFault("Simkl page token could not be encoded")}
	}
	response.NextPageToken = encoded
	return response
}

// loadStep reads Steps[0] and carries the rows after the last one handed out:
// emitNow rows for this page, plus as many as carryBudgetBytes allows.
func (s *Server) loadStep(ctx context.Context, acct account, token *pageToken, emitNow int) *pluginv1.WatchSyncFault {
	current := token.Steps[0]
	result, fault := s.readStep(ctx, acct, current)
	if fault != nil {
		return fault
	}
	rows, keys := sortRows(result.rows)
	fingerprint := listFingerprint(keys, result.complete)
	if !token.Started {
		token.Started = true
		token.Fingerprint = fingerprint
		token.warn(result.warnings...)
		if current.Read == readRatings {
			token.Complete = result.complete
		}
		if current.AnimeCursor != "" && result.animeRows {
			token.Next[cursorProgressAnime] = current.AnimeCursor
		}
	} else if token.Complete && fingerprint != token.Fingerprint {
		return temporaryFault(listChangedMessage)
	}

	start := 0
	for start < len(rows) {
		comparison := strings.Compare(keys[start], token.After)
		if comparison > 0 {
			break
		}
		if comparison == 0 {
			// Rows with the same key are identical, so skipping as many as
			// were handed out resumes at the right one.
			skipped := 0
			for start < len(rows) && keys[start] == token.After && skipped < token.AfterCount {
				start++
				skipped++
			}
			break
		}
		start++
	}
	end := start
	budget := s.carryBudget
	for end < len(rows) {
		if end-start >= emitNow {
			size := proto.Size(rows[end])
			if size > budget {
				break
			}
			budget -= size
		}
		end++
	}
	token.carried = rows[start:end]
	token.More = end < len(rows)
	return nil
}

// warn records import warnings for the traversal's final page. A re-read of
// an oversized list reports nothing new, so only a step's first read warns.
func (t *pageToken) warn(messages ...string) {
	if len(messages) == 0 {
		return
	}
	if t.Warnings == nil {
		t.Warnings = make(map[string]int)
	}
	for _, message := range messages {
		t.Warnings[message]++
	}
}

// warningMessages returns each recorded warning once, sorted. A repeated one
// reads "message (n items)", the way the host summarizes a run's repeated
// warnings, so a library with many skipped titles shows the count the
// built-in provider's run showed, unaffected by the host's cap on warnings
// per traversal.
func (t *pageToken) warningMessages() []string {
	var messages []string
	for message, count := range t.Warnings {
		if count > 1 {
			message = fmt.Sprintf("%s (%d items)", message, count)
		}
		messages = append(messages, message)
	}
	slices.Sort(messages)
	return messages
}

func (t *pageToken) handedOut(key string) {
	if key == t.After {
		t.AfterCount++
		return
	}
	t.After = key
	t.AfterCount = 1
}

func (t *pageToken) finishStep() {
	t.Steps = t.Steps[1:]
	t.Started = false
	t.More = false
	t.After = ""
	t.AfterCount = 0
	t.Fingerprint = ""
	t.carried = nil
}

// rowSortKey orders rows by provider item key, then by content, so equal keys
// mean identical rows.
func rowSortKey(row *pluginv1.WatchSyncRemoteState) string {
	encoded, _ := proto.MarshalOptions{Deterministic: true}.Marshal(row)
	digest := sha256.Sum256(encoded)
	return row.GetProviderItemKey() + "\x00" + hex.EncodeToString(digest[:])
}

func sortRows(rows []*pluginv1.WatchSyncRemoteState) ([]*pluginv1.WatchSyncRemoteState, []string) {
	type keyed struct {
		row *pluginv1.WatchSyncRemoteState
		key string
	}
	entries := make([]keyed, len(rows))
	for index, row := range rows {
		entries[index] = keyed{row: row, key: rowSortKey(row)}
	}
	slices.SortStableFunc(entries, func(a, b keyed) int { return strings.Compare(a.key, b.key) })
	sorted := make([]*pluginv1.WatchSyncRemoteState, len(entries))
	keys := make([]string, len(entries))
	for index, entry := range entries {
		sorted[index], keys[index] = entry.row, entry.key
	}
	return sorted, keys
}

func listFingerprint(keys []string, complete bool) string {
	hash := sha256.New()
	if complete {
		hash.Write([]byte{1})
	} else {
		hash.Write([]byte{0})
	}
	for _, key := range keys {
		hash.Write([]byte(key))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// encode packs the token as base64url(gzip(varint header length, JSON header,
// carried rows as a protobuf message)).
func (t *pageToken) encode() (string, error) {
	header, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	rows, err := proto.Marshal(&pluginv1.WatchSyncListRemoteStateResponse{Items: t.carried})
	if err != nil {
		return "", err
	}
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(binary.AppendUvarint(nil, uint64(len(header)))); err != nil {
		return "", err
	}
	if _, err := writer.Write(header); err != nil {
		return "", err
	}
	if _, err := writer.Write(rows); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer.Bytes()), nil
}

func decodePageToken(raw string, kind pluginv1.WatchSyncRemoteStateKind, cursor string) (*pageToken, *pluginv1.WatchSyncFault) {
	invalid := invalidRequestFault("Simkl page token is invalid")
	compressed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, invalid
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, invalid
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxTokenBytes+1))
	if err != nil || len(data) > maxTokenBytes {
		return nil, invalid
	}
	headerLength, prefix := binary.Uvarint(data)
	if prefix <= 0 || headerLength > uint64(len(data)-prefix) {
		return nil, invalid
	}
	header := data[prefix : prefix+int(headerLength)]
	var token pageToken
	if err := json.Unmarshal(header, &token); err != nil {
		return nil, invalid
	}
	var rows pluginv1.WatchSyncListRemoteStateResponse
	if err := proto.Unmarshal(data[prefix+int(headerLength):], &rows); err != nil {
		return nil, invalid
	}
	if token.Kind != int32(kind) || token.Cursor != cursor || len(token.Steps) == 0 || token.AfterCount < 0 {
		return nil, invalidRequestFault("Simkl page token does not belong to this traversal")
	}
	if token.Next == nil {
		token.Next = make(map[string]string)
	}
	token.carried = rows.GetItems()
	return &token, nil
}

// decodeCursor reads a traversal cursor. A cursor this plugin cannot read
// counts as none, so the traversal reads everything and writes a fresh one.
func decodeCursor(raw string) map[string]string {
	cursor := make(map[string]string)
	if strings.TrimSpace(raw) == "" {
		return cursor
	}
	if err := json.Unmarshal([]byte(raw), &cursor); err != nil {
		return make(map[string]string)
	}
	return cursor
}

func encodeCursor(cursor map[string]string) string {
	if len(cursor) == 0 {
		return ""
	}
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func pageSize(requested int32) int {
	if requested <= 0 {
		return defaultPageSize
	}
	return min(maxPageSize, int(requested))
}

func cloneMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
