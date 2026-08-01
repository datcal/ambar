# Ambar for Godot

Browse the library and import from inside the editor, with each import recorded back to the
server so §9.1 knows a project depends on the file and the credits file can be generated.

## Install

1. Copy the `ambar` folder into your project's `addons/` directory, so you have
   `res://addons/ambar/plugin.cfg`.
2. **Project → Project Settings → Plugins → Ambar → enable.**
3. An **Ambar** tab appears in the top bar next to *2D*, *3D* and *Script*. Open it.
4. Fill in the server address and an API token, then press **Save and test**. The panel says
   what happened — it does not just go quiet.

To make a token: **Create a token…** opens Ambar's *Settings → API tokens* page in your browser.
Tick **write**; the plugin needs it to record imports. Read alone browses but cannot import
properly.

Where the settings live, and why:

| | file | committed? |
|---|---|---|
| Server URL | `res://ambar.cfg` | yes — everyone on the team gets it with the project |
| API token | `user://ambar_token.cfg` | no — personal to your machine |

## Using it

* **Search** takes the same query language as the web UI: `sword`, `type:model`, `32x32`,
  `theme:sci-fi`, `-style:realistic`.
* **Import** downloads to `res://assets/<kind>/<pack>/<filename>`, records it in
  `res://.ambar/manifest.json` (commit this) and tells the server. A tile already in the project
  says *In project* instead.
* **Set pixel-art import defaults** (on the server panel) points this project's texture importer
  at nearest filtering, no mipmaps and lossless compression. Project-wide, once.
* **Credits** writes `res://CREDITS.md` from what this project has imported.

## If nothing appears

The plugin is enabled but there is no Ambar tab:

* Check **Project → Project Settings → Plugins** — an addon whose script fails to load shows as
  enabled but does nothing. The error is in the **Output** panel at the bottom of the editor.
* Godot 4.1 and older expose the editor API differently; `editor_compat.gd` handles both, but if
  the tab is missing the plugin falls back to a dock in the upper-left slot and logs a warning.

Searches return nothing:

* Press **Server…** and then **Save and test**. Every failure has its own message — cannot
  resolve the hostname, cannot connect, unauthorised, and so on.
* `http://` is added for you if you leave the scheme off.

Imports say "the server was not told":

* The token has no **write** scope. Make a new one with write ticked and paste it again — the
  files are already in the project, and the manifest keeps the record.
