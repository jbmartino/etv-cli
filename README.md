# etv

Go CLI to manage ErsatzTV channels, collections, schedules, and playouts from a YAML config.

Keep your setup in files and push it with `etv apply`. It works alongside the ErsatzTV web UI, not
instead of it, and only ever touches what you name. Because the files are the setup, they double as a
backup: if the server is rebuilt, `etv apply` puts your channels, collections, and schedules back.

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

Every channel setting is optional, and **omitting one means the manifest does not manage it**. Apply
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
    enabled: true
    showInEpg: true
```

`logo` has three states, and so does every nullable setting:

| you write | meaning |
|---|---|
| *omitted* | not managed, apply leaves the current value alone |
| `logo: ""` | managed and empty, apply removes the logo |
| `logo: logos/g4.png` | managed, apply uploads it if it differs |

## Commands

```
etv plan        show what apply would change (alias: diff)
etv apply       push your channel setup to the server
etv validate    check the schedules locally, no server needed
etv status      show what the server currently has
```

`apply` prints the plan and asks before it changes anything. Pass `-y` in CI or a git hook.

```sh
$ etv plan
validated 12 schedules
[dry-run] add to collection "Nickelodeon": CatDog (1998)
[dry-run] update channel "G4" (group)
[dry-run] upload schedule "mtv"
[dry-run] rebuild "MTV" (schedule changed)

4 change(s) would be applied
```

Applying an unchanged manifest changes nothing and rebuilds nothing, so it is safe to run on every
push.

```sh
$ etv apply
validated 12 schedules
already up to date
```

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

It **never quietly does nothing**. If the manifest asks for something apply cannot do, it fails and
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
