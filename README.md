# Simkl watch-provider plugin for Prairie

Connects Prairie profiles to [Simkl](https://simkl.com) through Prairie's `watch_sync_provider.v1` plugin contract. It replaces the Simkl provider that earlier Prairie releases built in, and keeps that provider's behavior so existing connections carry over.

Version 0.4.0 needs a Prairie server that supports plugin SDK v0.21, which adds dropped shows, the movie rating hold, and sync warnings to the plugin contract. The **Log rewatches** setting needs a Prairie server that supports connection settings from plugin SDK v0.24; an older server does not show it, and rewatches stay off.

## Capabilities

- Imports watched movies and episodes, anime included. Simkl keeps only the last watch of a title, so each title imports as one play.
- Imports resume progress for paused movies and episodes.
- Exports watched plays and unwatched titles. A play Simkl already holds at the same time, to the second, is not sent again.
- Imports and updates Simkl's plan-to-watch list as the Prairie watchlist, for movies and series. Simkl has no favorites list.
- Syncs dropped shows both ways, anime included. See [Dropped shows](#dropped-shows).
- Sends live playback start, pause, and stop events.
- Logs another play of a title the account already watched as a Simkl rewatch, when the profile turns on **Log rewatches**. See [Rewatches](#rewatches).
- Imports, sends, and clears movie and series ratings. See [Ratings](#ratings).
- Reports what an import skipped as warnings on the sync: titles Simkl returned without a usable id, ratings it could not prove complete, and titles removed from a Simkl list, which Prairie does not import.

## How it reads Simkl

Each sync first reads Simkl's `/sync/activities` and then reads only the lists whose timestamps moved since the last sync, starting from the last timestamp. The first sync reads everything. The plan-to-watch list is read in full on every sync, and a title missing from it counts as removed.

Simkl returns each list in one response. The plugin reads a list once and hands it to Prairie in pages of up to 100 titles. A list too large to hand over from one read, more than about 2,500 episodes or 5,000 movies, is read once more for each further part of that size. If the plan-to-watch list, the dropped lists, or a complete ratings read changes between those reads, the sync stops and the next sync starts over.

Warnings for the same reason are combined into one with a count, such as "simkl watched movie skipped because it has no usable external id (3 items)", which is how Prairie showed the built-in provider's warnings.

Writes are paced to one per second per profile, as Simkl requires. When Simkl rate-limits a request, the plugin waits and retries in place: one second for the per-second limit, five seconds for Simkl's short per-user write lock, at most twice. A daily quota, or a limit that persists, pauses the connection for the time Simkl gives, or one minute when it gives none.

## Ratings

Simkl rates from 1 to 10, the same scale the plugin contract uses.

Simkl keeps anime in its own lists. A rated anime entry marked as a movie imports as a movie, one marked as TV as a series, and any other or missing type as a series without its TMDB id, because that id can belong to an unrelated movie.

A ratings import reads Simkl's full movie, show, and anime rating lists whenever any of their timestamps moved. Prairie treats a rating missing from the read as removed on Simkl only when the read is complete for both movies and series. It is not complete when a rated title has no usable id, or when a rated anime entry does not say whether it is a movie or a series, which Simkl's rating lists usually leave out. While an account has such anime ratings, new and changed ratings still import, but removals on Simkl do not.

Rating a movie on Simkl files it as watched. The plugin therefore asks Prairie to hold a movie rating until the profile has watched the movie, as the built-in provider did, and sends it after that. Series ratings and rating removals of both kinds are sent right away.

## Rewatches

Simkl ignores a new play of a movie or episode the account already watched, unless the request asks for a rewatch. Each Prairie profile chooses with the **Log rewatches** switch on its Simkl connection, which is off by default, as Simkl asks of every app. When it is on, the plugin sends exported plays with `allow_rewatch=yes`, and Simkl records a play of a title the account already finished as a rewatch with its own date.

- Rewatches are a Simkl PRO and VIP feature. On a free account Simkl records the first watch of a title as usual and nothing for later plays.
- Rewatches travel with exported plays, so **Send watched changes** must be on as well. Live playback events do not carry the flag: Simkl warns that a flag on a playback start can log a rewatch of whatever played before.
- Simkl merges two watches of the same movie or episode less than two days apart into one, so a play Prairie sends again does not add a second rewatch.
- A play without a watch time is sent without the flag, because Simkl would date it at the time it arrives.
- Imports still bring in one play per title. Reading rewatch sessions back from Simkl is not supported yet.

## Dropped shows

Simkl keeps a dropped show in its "dropped" list. Dropping a show in Prairie moves it there; undropping moves it to Simkl's "watching" list. Neither move changes the show's watch history.

The plugin reads Simkl's dropped shows and dropped anime in full whenever either list's `all` timestamp in `/sync/activities` moved, so a show moved off the dropped list on Simkl is noticed too. Each read is the account's complete dropped set: a show missing from it is not dropped. While neither timestamp moves, the plugin skips the read and Prairie leaves its record of dropped shows as it is. Simkl records no time a show was dropped, so Prairie stamps an imported drop with the time of the sync.

## Setup

1. Create an app in your Simkl account's [developer settings](https://simkl.com/settings/developer/). Choose the **TV, devices & command line** type. The plugin does not use a client secret, and Simkl requires one from **Server apps & services** apps.
2. Install the plugin. In its settings, enter the app's client ID.
3. Connect each Prairie profile from its watch-provider settings. Prairie shows a code to enter at simkl.com/pin.

## Simkl AUTH V1 and AUTH V2

Simkl has two sign-in systems. Apps created in Simkl's developer settings now use AUTH V2. Older apps use AUTH V1, which Simkl plans to retire around April 2027. An app cannot move from one to the other: AUTH V2 needs a new app with its own client ID, and every profile signs in to it once.

The plugin works out which kind of app the client ID belongs to when a profile connects. An AUTH V2 app connects with Simkl's device codes and asks for read and write access. Its access tokens last seven days. Prairie renews an expiring token the next time it syncs, so a connection keeps working as long as Prairie uses it at least once every 180 days. An AUTH V1 app connects with PIN codes, and its tokens do not expire.

Simkl only accepts a token together with the client ID of the app that issued it. Each AUTH V2 connection records the app it signed in through and keeps using it. AUTH V1 connections use the **Client ID** setting. To move an install whose profiles connected through an AUTH V1 app:

1. Create an AUTH V2 app as described in [Setup](#setup).
2. Leave the AUTH V1 app's client ID in the **Client ID** setting, and enter the new app's client ID as the **AUTH V2 client ID**.
3. New connections sign in through the AUTH V2 app. Existing connections keep using the AUTH V1 app until the profile disconnects Simkl and connects it again.

Do not replace the AUTH V1 client ID with the AUTH V2 one while profiles are connected through the AUTH V1 app. The plugin would then send their tokens with a client ID that did not issue them.

## Upgrading from the built-in Simkl provider

Existing Simkl connections carry over once a Prairie server release that maps this plugin to the built-in `simkl` provider is installed. The plugin reuses the stored Simkl tokens, which are AUTH V1 tokens that do not expire, and produces the same item keys, so profiles do not reconnect and Prairie keeps its record of what was synced. If that server release does not copy the client ID from the old Simkl server setting, enter it in the plugin's settings.

The first sync after the upgrade reads the whole Simkl library once, because the plugin tracks its read position separately from the built-in provider. Plays, progress, ratings, and dropped shows Prairie already has are not imported twice. For the same reason, that sync warns that titles were removed from a Simkl list if the account ever removed one.

Playback events for different titles are no longer sent strictly one after another per profile, as the built-in provider sent them. Events for the same movie or series still are.

## Not yet supported

- Importing series rating removals while the account has anime ratings without a movie or TV type. The built-in provider still imported them. The plugin contract marks a ratings read complete for movies and series together, so the plugin imports removals of neither kind until a newer contract can tell them apart.
- Revoking an AUTH V2 sign-in on Simkl when a profile disconnects. The plugin contract has no disconnect call, so the connection stays listed in the account's [Connected Apps](https://simkl.com/settings/connected-apps/) until its owner removes it there.

## Development

```bash
make test
make build
./plugin manifest
```

`make build-all` produces static binaries for the platforms declared in `manifest.json`.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request. Changes to
authentication, reconciliation, idempotency, or the watch-sync contract should
start as an issue.

## License

AGPL-3.0-only.
