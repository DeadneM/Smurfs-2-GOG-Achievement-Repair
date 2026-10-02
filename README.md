![Smurfs 2 GOG Achievement Repair](docs/images/banner.png)

# The Smurfs 2: The Prisoner of the Green Stone - GOG Achievement Repair

Repairs GOG Galaxy achievements that should already be unlocked according to your local `flow.sav` progression, and includes an optional save helper for the Stolas Challenge-difficulty achievement.

This project targets the GOG release of **The Smurfs 2: The Prisoner of the Green Stone** (product ID `1906012961`).

## What it does

The tool reads the Unreal Engine `flow.sav`, compares provable progress with the live GOG achievement state, and offers to repair achievements that are missing even though their conditions are already met.

Statuses:

- `UNLOCKED` - already unlocked on GOG.
- `VERIFIED` - the save proves the requirement is met; automatic repair is available.
- `INFO` - the save does not keep reliable proof for this event-specific achievement; manual repair is available only if you genuinely earned it.
- `NOT MET` - the save proves the known requirement has not been completed.

`Smurfinator` is eligible for automatic repair only after every other GOG achievement is reported as unlocked.

## Main menu

Version 1.1 adds a simple English-only start menu:

```text
1) Audit / repair GOG achievements
2) Save helper: change difficulty per slot (Story / Epic / Challenge)
Q) Quit
```

## Save difficulty helper

Version 1.1.2 can change the **game difficulty independently for each used save slot**.

Public difficulty mapping confirmed for this game:

- **Story** = `EGameDifficulty::EASY`
- **Epic** = `EGameDifficulty::MEDIUM`
- **Challenge** = `EGameDifficulty::HARD`

`EGameDifficulty::HARD_PLUS` also exists internally in the executable, but it is not one of the three documented player-facing difficulty options and is deliberately not exposed by the tool.

This can help with **And his name is Zosimos...**, which requires defeating Stolas on **Challenge** difficulty. Project testing indicates that the safest route is to use Challenge for the **story encounter with Stolas**. The later optional replay/challenge version of the boss may not trigger the GOG achievement reliably. Afterward, the slot can be switched back to Story or Epic.

After changing difficulty with the helper, load the save once. If a level sits on an endless loading screen or its state appears stale, close the game, restart it, and load the save again before continuing.

Before every difficulty edit, the tool creates a complete `flow.sav` backup, reparses the modified save, verifies the selected slot, and restores the original automatically if post-write validation fails.

Command-line form:

```text
Smurfs2_GOG_Achievement_Repair.exe --set-difficulty <slot> <story|epic|challenge> [flow.sav]
```

## Achievement-repair safety

- In achievement mode, `flow.sav` is opened read-only and is never modified.
- GOG authentication tokens are never printed or written to the log.
- Unknown or missing achievement mappings are not written.
- `NOT MET` achievements are not offered by the manual `INFO` menu.
- A server write is shown as `[OK]` only after the tool re-reads the GOG account and confirms a non-null `date_unlocked`.
- GOG `date_unlocked: null` is never sent because that clears an achievement.

## Usage

1. Close the game.
2. Keep **GOG Galaxy open and signed in** for achievement repair.
3. Run `Smurfs2_GOG_Achievement_Repair.exe`.
4. Choose achievement repair or the optional difficulty helper.
5. In achievement repair mode, type `REPAIR` only when prompted for `VERIFIED` achievements.
6. In manual `INFO` mode, select only achievements you genuinely earned and confirm with `I EARNED THESE`.
7. Trust only `[OK]` entries that include a server-confirmed unlock date.

The normal save location is:

```text
%LOCALAPPDATA%\SM2\Saved\SaveGames\flow.sav
```

## Offline audit

```text
Smurfs2_GOG_Achievement_Repair.exe --offline-audit "path\to\flow.sav"
```

Offline mode cannot know which achievements are already unlocked on the live GOG account and cannot evaluate the live-account condition for `Smurfinator`.

## Command-line difficulty helper

```text
Smurfs2_GOG_Achievement_Repair.exe --set-difficulty <slot> <story|epic|challenge> [path\to\flow.sav]
```

Example:

```text
Smurfs2_GOG_Achievement_Repair.exe --set-difficulty 1 challenge
```

## Release packages

Download **v1.1.2** from the [GitHub release](https://github.com/DeadneM/Smurfs-2-GOG-Achievement-Repair/releases/tag/v1.1.2).

The release provides two clean archives:

- `Smurfs2_GOG_Achievement_Repair_v1.1.2_Windows_x64.zip` - EXE + README
- `Smurfs2_GOG_Achievement_Repair_v1.1.2_Script.zip` - Go script + README

## Build

The project uses only the Go standard library.

```text
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
go build -trimpath -ldflags="-s -w" -o Smurfs2_GOG_Achievement_Repair.exe Smurfs2_GOG_Achievement_Repair.go
```

## Version 1.1.2

- English-only console UI.
- `REPAIR` replaces the old French `REPARER` command.
- `I EARNED THESE` is the single manual confirmation phrase.
- User-Agent version strings now match the real release version.
- Added the optional fail-closed per-slot Story / Epic / Challenge save helper.
- Added backup and post-write save validation for the only feature that modifies `flow.sav`.
- Existing GOG unlock semantics remain unchanged from the successfully tested 1.0.0 path.

## Disclaimer

Use the manual repair option responsibly. This project is an unofficial community tool and is not affiliated with Microids, OSome Studio, Peyo Company, or GOG.
