# Ambar for Godot

Browse your [Ambar](https://github.com/datcal/ambar) asset library from inside the Godot
editor and import from it, with every import recorded back to the server — so the library
knows a project depends on that file, and can generate the credits for you.

Requires Godot 4. Verified on 4.7.

## Install

1. Unzip this at the root of your Godot project, so you have
   `res://addons/ambar/plugin.cfg`.
2. **Project → Project Settings → Plugins → Ambar → enable.**
3. An **Ambar** tab appears in the top bar next to *2D*, *3D* and *Script*. Open it.
4. Fill in the server address and an API token, then press **Save and test**. The panel
   tells you what happened — it does not just go quiet.

To make a token: **Create a token…** opens Ambar's *Settings → API tokens* page in your
browser. Tick **write** — the plugin needs it to record imports and to store model
previews. A read-only token can browse but not much else.

Where the settings live, and why:

| | file | committed? |
|---|---|---|
| Server URL | `res://ambar.cfg` | yes — everyone on the team gets it with the project |
| API token | `user://ambar_token.cfg` | no — personal to your machine |
| Grid preferences | `user://ambar_prefs.cfg` | no — thumbnail size, sort, page size |

## Library

Search takes the same query language as the web UI: `sword`, `type:model`, `32x32`,
`theme:sci-fi`, `-style:realistic`.

- The grid reflows to the window; thumbnails are 64 to 256 pixels, and the choice is
  remembered.
- Nine sort orders, fetched from the server, and numbered pages.
- One tile per *asset*, not per file: a sprite shipped as PNG, PSD and ASEPRITE is one
  tile marked "3 formats".
- Click a tile and the panel on the right shows it properly — full-size preview,
  dimensions, frame count or geometry or duration, pack, licence, tags, and the other
  formats, with the one you import selectable. Double-click imports.
- **Models with no thumbnail are rendered here.** The server has no renderer, so most
  models have no picture until something draws one. The editor is a renderer: the plugin
  draws them as you browse and posts the result back, which fills the web UI in too.
  `.fbx` is the exception — nothing in this chain can read it without Blender, and the
  panel says so rather than showing an empty box.

**Import** downloads to `res://assets/<kind>/<pack>/<filename>`, checks the bytes against
the hash the server advertised, records it in `res://.ambar/manifest.json` — commit that
file — and tells the server.

## In this project

The second tab is what this project has taken from the library, and whether it is still
right. It compares the committed manifest against the server's record, and every
disagreement has a name:

| | |
| --- | --- |
| *library has a newer version* | **Update** re-downloads over the same path, so scenes pointing at it keep working |
| *not recorded on the server* | imported while the server was unreachable — **Sync** replays it |
| *missing from this project* | the manifest describes a file that is no longer in the checkout |
| *gone from the library* | the asset it came from is missing at the source |

**Remove** deletes this project's copy, forgets the manifest entry and releases the
server's record, behind a confirmation that names the file. It never touches the library.

**Write CREDITS.md** builds `res://CREDITS.md` from what this project actually uses,
grouped by licence.

## Import defaults for pixel art

On the server panel, **Set pixel-art import defaults** points this project's texture
importer at nearest filtering, no mipmaps and lossless compression, project-wide and
once. Godot's defaults smooth pixel art, and per-file fixes do not survive a re-import.

## If something is wrong

**The plugin is enabled but there is no Ambar tab.** An addon whose script fails to load
shows as enabled and does nothing; the error is in the **Output** panel at the bottom of
the editor. On Godot versions with no editor main screen the plugin falls back to a dock
in the upper-left and logs a warning.

**Searches return nothing.** Press **Server…**, then **Save and test**. Every failure has
its own message — cannot resolve the hostname, cannot connect, unauthorised, and so on.
`http://` is added for you if you leave the scheme off.

**The sort list only offers one order**, or the grid is empty until you re-test the
connection: the server is older than the plugin. `/api/v1/sorts` arrived in v1.0.0.

**Imports say "the server was not told".** The token has no **write** scope. Make a new
one with write ticked and paste it again — the files are already in the project and the
manifest kept the record, so the **In this project** tab will offer to **Sync** them.

## Licence

MIT, same as the server.
