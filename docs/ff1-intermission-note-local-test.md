# FF1 Intermission Note Local Test Runbook

This runbook captures the current local test setup for intermission notes on FF1, including:

- the FF1 device-local player setup
- the temporary cast shim on port `1111`
- how to cast a signed playlist to the device
- how to run the local `dp1-feed-v2` publisher form
- how to test the full authoring -> export -> playback loop

## Current FF1 setup

For local testing, the FF1 should be in this state:

- local player URL on device: `http://127.0.0.1:8314`
- temporary cast shim on device: `http://<ff1-hostname>.local:1111/api/cast`

Current expected service state on the FF1:

- `ff1-cast-shim.service`: active
- `feral-controld.service`: inactive

Check it with:

```bash
ssh feralfile@<ff1-hostname>.local 'systemctl --user --no-pager --plain status ff1-cast-shim feral-controld'
```

## Revert the FF1 to normal control service

If you want to disable the temporary cast shim and restore the stock control service:

```bash
ssh feralfile@<ff1-hostname>.local '
  systemctl --user stop ff1-cast-shim
  systemctl --user start feral-controld
'
```

## Start local `dp1-feed-v2`

The publisher form lives in the local `dp1-feed-v2` server.

### Recommended local start

```bash
cd /home/mosko/Work/dp1-feed-v2
make publisher
```

What `make publisher` does:

- creates `config/config.yaml` from the local example if needed
- generates a local signing key if the placeholder is still present
- uses local Postgres if it is usable
- otherwise falls back to an isolated Docker Postgres
- starts the server

### Open the publisher UI

Open:

- `http://localhost:8787/publisher`

## Publisher flow to test

The intended local test flow is:

1. click `Use Local Test Account`
2. verify wallet address
3. optionally verify ENS name
4. create test playlist
5. add playlist-level note and/or item-level note
6. save the note
7. export the playlist JSON
8. cast that exported playlist to the FF1

## Notes on identity/proofs

Current publisher auth model:

- local prototype login: `Use Local Test Account`
- longer-term auth direction still includes passkeys; they are just not the recommended local test path right now
- required public proof before publishing: at least one verified proof
- currently implemented proofs:
  - wallet address
  - ENS name, verified against a linked wallet

ENS verification uses the same resolver-service model as `ff-app`, not raw onchain RPC by default.

## Exporting playlist JSON from the publisher UI

The `/publisher` page includes:

- `Download Playlist JSON`
- `Copy Playlist JSON`
- `Create Test Playlist`
- `Copy ff1 Command`

Use `Download Playlist JSON` to get the exact playlist you want to send to FF1.

Note:

- you may still need to adjust the file path
- you may still need to adjust the device name
- the UI now auto-creates a minimal publisher-owned test channel behind the scenes when you create a new playlist, so note saves are allowed

## Casting an exported playlist file to FF1

Recommended path:

1. use `Download Playlist JSON`
2. click `Copy ff1 Command`
3. adjust the file path or device name if needed
4. run it in the terminal

Example if you downloaded a playlist file to:

- `/tmp/my-exported-playlist.json`

send it like this:

```bash
ff1 send "/tmp/my-exported-playlist.json" -d office
```

If validation blocks the send during local prototype testing, you can temporarily use:

```bash
ff1 send "/tmp/my-exported-playlist.json" -d office --skip-verify
```

Known caveat:

- `ff1 send` currently has a validation mismatch with the exported prototype playlists around item `created` / signature expectations
- the playback and note-rendering path on FF1 has still been proven with the current local setup

## Current architecture caveat

The FF1 is currently using a temporary shim on port `1111`.

Why:

- the stock `feral-controld` path appears to strip additive DP fields like `note`
- the shim preserves raw `dp1_call` and forwards it directly to Chromium over CDP

This means:

- playback testing is valid
- authoring/export/playback testing is valid
- but the upstream `feral-controld` bug is still real and should still be fixed separately

## Files relevant to the current local setup

### `ff-player`

- `/home/mosko/Work/ff-player/src/app/playlist/playlist-client.tsx`
- `/home/mosko/Work/ff-player/src/components/note-card/NoteCard.tsx`
- `/home/mosko/Work/ff-player/scripts/package-local-export.sh`

### `ffos-user`

- `/home/mosko/Work/ffos-user/components/feral-setupd/src/main.rs`
- `/home/mosko/Work/ffos-user/components/feral-setupd/src/constant.rs`
- `/home/mosko/Work/ffos-user/components/feral-connectd/cmd/castshim/main.go`
- `/home/mosko/Work/ffos-user/users/feralfile/scripts/install-local-player.sh`
- `/home/mosko/Work/ffos-user/users/feralfile/scripts/serve-local-player.sh`

### `dp1-feed-v2`

- `/home/mosko/Work/dp1-feed-v2/internal/httpserver/publisher_console.go`
- `/home/mosko/Work/dp1-feed-v2/internal/publisherauth/service.go`
- `/home/mosko/Work/dp1-feed-v2/internal/httpserver/handlers.go`

## If you need to inspect FF1 logs

Chromium log:

```bash
ssh feralfile@<ff1-hostname>.local 'tail -n 200 /home/feralfile/.logs/chromium.log'
```

Cast shim / device services:

```bash
ssh feralfile@<ff1-hostname>.local 'journalctl --user -u ff1-cast-shim -u feral-controld -n 200 --no-pager'
```

## Proven current result

On the standard local `/api/cast` path now active on the FF1, the device has already rendered:

- playlist-level note
- item-level note
- then artwork

So the current setup is valid for real local testing of the note experience on FF1 hardware.

## Optional current-device example

If you want the exact current device values from this test session:

- FF1 hostname: `ff1-03vdu3x1.local`
