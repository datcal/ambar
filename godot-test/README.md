# Testing the Godot plugin

The editor plugin is the one part of Ambar the Go suite cannot reach, and This directory exists to prevent one specific
failure: the plugin once shipped with a GDScript *parse* error, which
leaves an addon enabled and completely inert with the only evidence in the Output panel. From the
outside, installing it did nothing.

None of this needs a display except `test_ui.gd`, and none of it needs the editor open.

    make godot-test GODOT="/path/to/godot"

`addons/` is a symlink to `../addons`, so the plugin under test is always the working copy.

## The passes

| Pass | What it proves | Needs |
| --- | --- | --- |
| `--headless --editor --quit` | every script parses and the plugin loads | nothing |
| `test_open.gd` | that opening the tab shows the library — parent, `set_plugin`, wait, and nothing else. Every other pass calls `_do_search()` itself, which is how a panel that opened empty for a whole milestone went unnoticed | a running Ambar |
| `test_api.gd` | the API client works against a real server: grouped search, sort orders, numbered pages, asset detail with variants and licence, thumbnails and previews that decode, and a bad token that explains itself | a running Ambar |
| `test_import.gd` | the import path end to end — search, select, press Import, and then the file is in the project, the manifest recorded it, and the panel says so | a running Ambar |
| `test_model.gd` | the plugin draws a model the server has no picture of, and stores it — fetch `preview.glb`, render it, check it is not blank, upload, fetch it back, and confirm a second upload is refused rather than duplicated | a running Ambar and a display |
| `test_project.gd` | the "In this project" screen: two imports, then the states it exists to show — an import the server was never told about and the Sync that replays it, an asset the library has moved on from and the Update that fixes it, a file deleted from the checkout, and a confirmed removal that leaves the library alone | a running Ambar |
| `test_ui.gd` | the panel renders: grid reflow, pager, inspector. Saves a PNG to look at | a display |

## Configuring it

The harnesses read the same two files the plugin does, so set them once:

    godot-test/ambar.cfg                      [server] base_url="http://127.0.0.1:8080"
    ~/.local/share/godot/app_userdata/AmbarPluginTest/ambar_token.cfg
                                              [server] token="ambar_…"

Both are gitignored. Make a token in Ambar under **Settings → API tokens**; the plugin's setup
panel has a button that opens that page.

## Two traps worth knowing before you write another one

**GDScript lambdas capture locals by value.** `var done = false` and `done = true` inside a
callback sets the *copy*, so a harness that waits on it waits forever. Mutate a shared Array
instead — `out.append(...)` — which is what `await_call` does.

**The root Window is not in the tree until the first frame.** An `HTTPRequest` refuses to run
outside the tree, so a `_initialize()` that fires a request immediately gets nothing back.
`await process_frame` first. `api.gd` reports that condition as an internal error rather than
blaming the URL, which is the only reason it took a minute to find.

**Wait on conditions, never on a duration.** An `HTTPRequest` is polled once per frame and a
window the compositor has throttled runs at a few frames a second, so a wall-clock wait
screenshots "Searching…" — while a frame count is forty seconds on one run and four minutes on
the next. `test_ui.gd` has `_until(condition, seconds)` for this.

**Parent the panel *and* give it a size.** `set_anchors_and_offsets_preset(PRESET_FULL_RECT)` is
what plugin.gd does when it hands the control to the editor's main screen; a harness that forgets
it gets a 0×0 control whose containers all collapse to their minimum. Every assertion still
passes — the nodes are there and correct — and the screenshot is a header over an empty page.

**Do not inherit the plugin's saved preferences.** The kind filter, sort and tile size live in
`user://ambar_prefs.cfg` and persist between runs; the import pass once failed with "0 tiles"
because an earlier session had left the filter on `model` while it searched for a sprite. Set
what the test depends on, explicitly.
