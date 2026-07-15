# etv

Go CLI to manage ErsatzTV channels, collections, schedules, and playouts from a YAML config.

Keep your setup in files and push it with `etv import`. It works alongside the ErsatzTV web UI, not
instead of it, and only ever touches what you name. Because the files are the setup, they double as a
backup: if the server is rebuilt, `etv import` puts your channels, collections, and schedules back.

Everything happens over the ErsatzTV HTTP API. There is no SSH, no `scp`, no `docker cp`, and no
volume mount, so the same binary drives a desktop, a container, or a cluster without knowing which.

## Install

```sh
go install github.com/jbmartino/etv-cli@latest
```

## Configure

Two environment variables, or the equivalent `--url` and `--key` flags:

```sh
export ETV_URL=http://localhost:8409
export ETV_API_KEY=...          # the server's ETV_API_KEY
```

## The manifest

`etv.yaml` lists the channels etv manages. Schedules are separate files, referenced by name.

```yaml
schedulesDir: schedules         # default: ./schedules

collections:
  - name: G4
    shows:
      - Attack of the Show
      - X-Play

channels:
  - name: G4
    schedule: g4                # -> schedules/g4.seq.yaml
    logo: logos/g4.png
```

A layout to match:

```
etv.yaml
schedules/g4.seq.yaml
logos/g4.png
```

Show titles are matched against the library. The server reports them as `Attack of the Show (2004)`,
so the bare title works too, as long as it is unambiguous. A title that matches nothing is an error,
not a warning.

### Channel settings

Every channel setting is optional, and **omitting one means the manifest does not manage it**. Import
leaves whatever the server has. That is what makes it safe to adopt existing channels without
writing down all thirty fields just to avoid resetting them.

```yaml
channels:
  - name: G4
    schedule: g4
    logo: logos/g4.png
    number: "5"
    group: RetroTV
    ffmpegProfile: nvenc        # by name
    streamingMode: TransportStreamHybrid
    transcodeMode: OnDemand
    idleBehavior: StopOnDisconnect
    preferredAudioLanguage: eng
    watermark: corner-bug       # by name
    filler: commercials         # by name
    mirrorSourceChannel: G4 HD  # by name or number
    enabled: true
    showInEpg: true
```

That is a sample, not the whole list: the manifest covers the channel settings that survive a
rebuilt server, including categories, slug seconds, the stream selector, playout source/mode/offset,
subtitle and music-video credit settings, and the song video mode.

`ffmpegProfile`, `watermark`, `filler` and `mirrorSourceChannel` are references. The server stores
each by a numeric id that a rebuilt server would not reuse, so the manifest names them and import
resolves the name against the server (watermarks and fillers via `GET /api/watermarks` and
`GET /api/fillers`). A name that does not exist is an error, not a silent skip.

`logo` has three states, and so does every nullable setting:

| you write | meaning |
|---|---|
| *omitted* | not managed, import leaves the current value alone |
| `logo: ""` | managed and empty, import removes the logo |
| `logo: logos/g4.png` | managed, import uploads it if it differs |

## Commands

```
etv plan        show what import would change (alias: diff)
etv import      push your channel setup to the server (alias: apply)
etv export      write the server's setup to a manifest directory
etv guide       show what is on now and next, per channel
etv validate    check the schedules locally, no server needed
etv status      show what the server currently has
```

`etv guide` reads the server's own XMLTV, which always matches the stream. Plex caches its own copy
that can lag after a playout rebuild, so when Plex and the picture disagree, `etv guide` is the
tie-breaker.

`import` prints the plan and asks before it changes anything. Pass `-y` in CI or a git hook. The
command is also available as `apply`, so an existing git hook or muscle memory keeps working.

```sh
$ etv plan
validated 12 schedules
[dry-run] add to collection "Nickelodeon": CatDog (1998)
[dry-run] update channel "G4" (group)
[dry-run] upload schedule "mtv"
[dry-run] rebuild "MTV" (schedule changed)

4 change(s) would be applied
```

Importing an unchanged manifest changes nothing and rebuilds nothing, so it is safe to run on every
push.

```sh
$ etv import
validated 12 schedules
already up to date
```

## Backing up

`etv export` reads a running server and writes the manifest that would reproduce it: `etv.yaml`, the
schedule files, and the logos. Point `import` at that directory later and a rebuilt server comes back.

```sh
etv export -o backup/           # writes backup/etv.yaml, backup/schedules/, backup/logos/
etv import -f backup/etv.yaml   # restore onto a fresh server
```

Export refuses to overwrite an existing `etv.yaml` unless you pass `--force`. It writes only what the
manifest can faithfully round-trip: a channel whose playout is not a single managed Sequential
schedule is reported and skipped rather than written as something that would not come back the same.
References (`ffmpegProfile`, `watermark`, `filler`, `mirrorSourceChannel`) are written by name, so the
backup does not depend on the server's internal ids surviving.

## What it will and will not do

It **updates what you name**. Channels are created and updated, collections have shows added and
removed, schedules are uploaded when their contents differ, and a playout is created, repointed, or
rebuilt so that each channel runs exactly the schedule you named.

It **never deletes what you did not name**. Anything on the server that is not in the manifest is
reported and left alone:

```
not in the manifest, left alone:
  channel "MTV" (number 13)
  collection "MTV"
  schedule "mtv"
```

A partial manifest is a normal thing to have, and a tool that pruned by default would be one typo
away from taking every channel off the air. If you want those gone, delete them yourself.

It **never quietly does nothing**. If the manifest asks for something import cannot do, it fails and
says why. A tool that silently no-ops is worse than one that errors, because you believe it.

### Renaming

Channels are matched by name, or by `number` when you give one. Give your channels a number if you
ever intend to rename them, otherwise a rename reads as "create a new channel".

## Requirements

Needs an ErsatzTV with the management API, including `PUT /api/schedules/{name}` and
`GET /api/collections/{id}/items`. Without the schedules API a schedule file has to be placed on the
server by hand, which is the whole thing this tool exists to avoid.

## Development

```sh
go test ./...
```

The apply logic is tested against an in-memory fake ErsatzTV that applies the same create-time
defaults the real server does, so the tests catch a partial update that would reset a channel.
