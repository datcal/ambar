@tool
extends VBoxContainer
## "In this project": what has been imported, and whether it is still right.
##
## The manifest at `res://.ambar/manifest.json` has existed since M9 and has never been visible.
## It is the answer to "where did this file come from", the input to the credits file, and — because
## it is committed — how each person's editor knows what the others already imported. All of that
## was true and none of it was on screen: the only trace of an import was a tile in the library grid
## going grey three pages into a search.
##
## Two sources, and the difference between them is the point:
##
##   the manifest   what this checkout believes it has, offline, instantly
##   the server     what the library believes this project has, plus the current content hash
##
## An entry in one and not the other is not an error, it is information. In the manifest only means
## the import happened while the server was unreachable — the specification promised that was replayable and this
## is where it gets replayed. On the server only means somebody else's checkout imported it without
## committing the manifest. Hashes that differ mean the library has moved on since.

signal changed # something was updated or removed; the library grid's badges are now stale

const Api := preload("res://addons/ambar/api.gd")
const Config := preload("res://addons/ambar/config.gd")
const Project := preload("res://addons/ambar/project.gd")
const Compat := preload("res://addons/ambar/editor_compat.gd")
const Importer := preload("res://addons/ambar/importer.gd")

const THUMB := 48

var _api_factory: Callable
var _plugin: EditorPlugin

var _heading: Label
var _status: Label
var _rows: VBoxContainer
var _sync_button: Button
var _confirm: ConfirmationDialog
var _pending_removal: Dictionary = {}

# Thumbnails, throttled the same way the grid throttles them.
var _queue: Array = []
var _in_flight := 0
const MAX_IN_FLIGHT := 4


func setup(api_factory: Callable, plugin: EditorPlugin) -> void:
	_api_factory = api_factory
	_plugin = plugin
	size_flags_vertical = Control.SIZE_EXPAND_FILL
	size_flags_horizontal = Control.SIZE_EXPAND_FILL

	var bar := HBoxContainer.new()
	add_child(bar)

	_heading = Label.new()
	_heading.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	bar.add_child(_heading)

	var refresh := Button.new()
	refresh.text = "Refresh"
	refresh.pressed.connect(reload)
	bar.add_child(refresh)

	_sync_button = Button.new()
	_sync_button.text = "Sync with server"
	_sync_button.tooltip_text = "Replay imports the server was never told about"
	_sync_button.pressed.connect(_sync)
	bar.add_child(_sync_button)

	var credits := Button.new()
	credits.text = "Write CREDITS.md"
	credits.tooltip_text = "Build res://CREDITS.md from what this project uses"
	credits.pressed.connect(_write_credits)
	bar.add_child(credits)

	_status = Label.new()
	_status.modulate = Color(1, 1, 1, 0.7)
	add_child(_status)

	var scroll := ScrollContainer.new()
	scroll.size_flags_vertical = Control.SIZE_EXPAND_FILL
	scroll.horizontal_scroll_mode = ScrollContainer.SCROLL_MODE_DISABLED
	add_child(scroll)

	_rows = VBoxContainer.new()
	_rows.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	scroll.add_child(_rows)

	# Removing an import deletes a file out of somebody's project, so it asks first and names
	# the file it is about to delete. The library is never touched by any of this.
	_confirm = ConfirmationDialog.new()
	_confirm.title = "Remove from this project"
	_confirm.ok_button_text = "Delete the file"
	_confirm.confirmed.connect(_do_removal)
	add_child(_confirm)


## reload rebuilds the screen: the manifest first, so it is never empty while a request is out,
## then the server's view merged on top.
func reload() -> void:
	var manifest := Project.manifest()
	_render(_merge(manifest, []), true)

	if not Config.configured():
		_status.text = "Not connected — showing the manifest only."
		return

	_status.text = "Checking with the server…"
	_api().project_uses(Project.uuid(), func(ok, result):
		if not ok:
			_status.text = "Showing the manifest only — %s" % str(result)
			return
		var uses: Array = result.get("uses", []) if result is Dictionary else []
		_render(_merge(Project.manifest(), uses), false)
	)


func _api() -> RefCounted:
	return _api_factory.call()


# --- merging the two views ------------------------------------------------------------

## _merge builds one row per asset id from both sides. `manifest_only` means the server has not
## answered yet, so nothing should be labelled "not recorded" — an unanswered request is not an
## absence.
func _merge(manifest: Dictionary, uses: Array) -> Array:
	var rows: Dictionary = {}

	for key in manifest:
		var entry: Dictionary = manifest[key] if manifest[key] is Dictionary else {}
		rows[int(String(key).to_int())] = {
			"asset_id": int(String(key).to_int()),
			"filename": String(entry.get("filename", "")),
			"res_path": String(entry.get("res_path", "")),
			"pack": String(entry.get("pack", "")),
			"imported_sha256": String(entry.get("sha256", "")),
			"in_manifest": true,
			"on_server": false,
		}

	for raw in uses:
		if not raw is Dictionary:
			continue
		var use: Dictionary = raw
		var id := int(use.get("asset_id", 0))
		var row: Dictionary = rows.get(id, {
			"asset_id": id, "in_manifest": false,
			"imported_sha256": String(use.get("imported_sha256", "")),
		})
		row["on_server"] = true
		row["use_id"] = int(use.get("id", 0))
		row["sha256"] = String(use.get("sha256", ""))
		row["outdated"] = bool(use.get("outdated", false))
		row["missing"] = bool(use.get("missing", false))
		row["size"] = int(use.get("size", 0))
		row["kind"] = String(use.get("kind", ""))
		# The server's copy of these is authoritative for anything the manifest lacks, but the
		# manifest's res_path is where *this* checkout actually put the file.
		if String(row.get("filename", "")) == "":
			row["filename"] = String(use.get("filename", ""))
		if String(row.get("res_path", "")) == "":
			row["res_path"] = String(use.get("res_path", ""))
		if String(row.get("pack", "")) == "":
			row["pack"] = String(use.get("pack", ""))
		rows[id] = row

	var out: Array = []
	for id in rows:
		out.append(rows[id])
	out.sort_custom(func(a, b): return String(a.get("filename", "")) < String(b.get("filename", "")))
	return out


## _state names what is wrong with one row, worst first, and returns ["label", colour]. An empty
## label means nothing is wrong.
func _state(row: Dictionary, manifest_only: bool) -> Array:
	var res_path := String(row.get("res_path", ""))
	if res_path != "" and not FileAccess.file_exists(res_path):
		# The file was deleted from the project without the plugin knowing — a normal thing to
		# happen in a checkout, and the manifest is now describing something that is not there.
		return ["missing from this project", Color(1, 0.55, 0.45)]
	if bool(row.get("missing", false)):
		return ["gone from the library", Color(1, 0.75, 0.4)]
	if bool(row.get("outdated", false)):
		return ["library has a newer version", Color(1, 0.85, 0.4)]
	if not manifest_only and not bool(row.get("on_server", false)):
		return ["not recorded on the server", Color(0.65, 0.8, 1)]
	if not bool(row.get("in_manifest", false)):
		return ["on the server, not in this checkout", Color(0.7, 0.7, 0.8)]
	return ["", Color(1, 1, 1)]


# --- drawing ---------------------------------------------------------------------------

func _render(rows: Array, manifest_only: bool) -> void:
	_queue.clear()
	# Detached *then* freed. queue_free alone defers to the end of the frame, so the old rows are
	# still children while the new ones are being added — one reload draws every row twice, and
	# anything reading the list in between sees both.
	for child in _rows.get_children():
		_rows.remove_child(child)
		child.queue_free()

	var unrecorded := 0
	var bytes := 0
	for raw in rows:
		var row: Dictionary = raw
		bytes += int(row.get("size", 0))
		if not manifest_only and bool(row.get("in_manifest", false)) and not bool(row.get("on_server", false)):
			unrecorded += 1
		_add_row(row, manifest_only)

	_heading.text = "%s — %d asset%s%s" % [
		Project.name_hint(), rows.size(), "" if rows.size() == 1 else "s",
		("  ·  %s" % _bytes(bytes)) if bytes > 0 else "",
	]
	_sync_button.disabled = unrecorded == 0
	_sync_button.text = "Sync with server" if unrecorded == 0 else "Sync %d with server" % unrecorded

	if rows.is_empty():
		_status.text = "Nothing imported yet. Find something in the Library tab and import it."
	elif manifest_only:
		pass # reload() sets its own message
	elif unrecorded > 0:
		_status.text = "%d import%s the server was never told about — Sync records them." % [
			unrecorded, "" if unrecorded == 1 else "s"]
	else:
		_status.text = "Project %s" % Project.uuid()

	_pump()


func _add_row(row: Dictionary, manifest_only: bool) -> void:
	var line := HBoxContainer.new()
	line.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_rows.add_child(line)

	var picture := TextureRect.new()
	picture.custom_minimum_size = Vector2(THUMB, THUMB)
	picture.expand_mode = TextureRect.EXPAND_IGNORE_SIZE
	picture.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED
	picture.texture_filter = CanvasItem.TEXTURE_FILTER_NEAREST
	line.add_child(picture)
	_queue.append([int(row.get("asset_id", 0)), picture])

	var text := VBoxContainer.new()
	text.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	line.add_child(text)

	var name_label := Label.new()
	name_label.text = String(row.get("filename", "?"))
	name_label.text_overrun_behavior = TextServer.OVERRUN_TRIM_ELLIPSIS
	text.add_child(name_label)

	var where := Label.new()
	var pack := String(row.get("pack", ""))
	where.text = String(row.get("res_path", "")) + ("   ·   " + pack if pack != "" else "")
	where.modulate = Color(1, 1, 1, 0.55)
	where.text_overrun_behavior = TextServer.OVERRUN_TRIM_ELLIPSIS
	text.add_child(where)

	var state := _state(row, manifest_only)
	var badge := Label.new()
	badge.text = String(state[0])
	badge.modulate = state[1]
	badge.custom_minimum_size.x = 200
	badge.horizontal_alignment = HORIZONTAL_ALIGNMENT_RIGHT
	line.add_child(badge)

	var show := Button.new()
	show.text = "Show"
	show.tooltip_text = "Select it in the FileSystem dock"
	show.disabled = not FileAccess.file_exists(String(row.get("res_path", "")))
	show.pressed.connect(func(): Compat.reveal(_plugin, String(row.get("res_path", ""))))
	line.add_child(show)

	if bool(row.get("outdated", false)):
		var update := Button.new()
		update.text = "Update"
		update.tooltip_text = "Download the library's current version over this copy"
		update.pressed.connect(func(): _update(row, update))
		line.add_child(update)

	var remove := Button.new()
	remove.text = "Remove"
	remove.tooltip_text = "Delete this project's copy and forget it. The library is untouched."
	remove.pressed.connect(func(): _ask_removal(row))
	line.add_child(remove)


# --- thumbnails --------------------------------------------------------------------

func _pump() -> void:
	while _in_flight < MAX_IN_FLIGHT and not _queue.is_empty():
		var job: Array = _queue.pop_front()
		_fetch_thumb(int(job[0]), job[1])


func _fetch_thumb(asset_id: int, into: TextureRect) -> void:
	if asset_id <= 0 or not is_instance_valid(into) or not Config.configured():
		_pump()
		return
	_in_flight += 1
	var api: RefCounted = _api()
	# Weak, because reload() frees every row and Godot will not call a lambda that has captured
	# a freed object — the callback would be skipped entirely, _in_flight included.
	var target := weakref(into)
	api.fetch_bytes(api.thumb_url(asset_id), func(ok, bytes):
		_in_flight -= 1
		var picture: Variant = target.get_ref()
		if ok and picture != null:
			var image := Image.new()
			var err := image.load_webp_from_buffer(bytes)
			if err != OK:
				err = image.load_png_from_buffer(bytes)
			if err == OK:
				picture.texture = ImageTexture.create_from_image(image)
		_pump()
	)


# --- actions -------------------------------------------------------------------------

## _sync replays the manifest entries the server has no row for. the specification: "tolerate being offline —
## the manifest is committed, so a later reconcile can replay it." This is that reconcile; before
## it, an import made while the NAS was asleep was invisible to the credits file for ever.
func _sync() -> void:
	if not Config.configured():
		_status.text = "Not connected."
		return
	var manifest := Project.manifest()
	_status.text = "Syncing…"

	_api().project_uses(Project.uuid(), func(ok, result):
		if not ok:
			_status.text = "Could not reach the server — %s" % str(result)
			return
		var known: Dictionary = {}
		for raw in (result.get("uses", []) as Array):
			if raw is Dictionary:
				known[int(raw.get("asset_id", 0))] = true

		var pending: Array = []
		for key in manifest:
			var id := int(String(key).to_int())
			if not known.has(id):
				pending.append([id, manifest[key]])
		if pending.is_empty():
			_status.text = "Already in step with the server."
			return

		var done := [0]
		for job in pending:
			var entry: Dictionary = job[1] if job[1] is Dictionary else {}
			_api().record_use(Project.uuid(), Project.name_hint(), int(job[0]),
				String(entry.get("res_path", "")), String(entry.get("sha256", "")),
				func(_sent, _message):
					# The array is mutated rather than reassigned: a GDScript lambda captures
					# locals by value, so `done += 1` would count in a copy nobody reads.
					done[0] += 1
					if done[0] == pending.size():
						_status.text = "Recorded %d import%s on the server." % [
							pending.size(), "" if pending.size() == 1 else "s"]
						reload()
			)
	)


func _update(row: Dictionary, button: Button) -> void:
	button.disabled = true
	button.text = "…"
	_status.text = "Updating %s…" % String(row.get("filename", ""))
	# Weak, because a successful update reloads the screen and frees this button — and a lambda
	# holding a freed object is one Godot refuses to call at all.
	var pressed := weakref(button)
	Importer.update_asset(_api(), row, func(ok, message):
		_status.text = message if ok else "Update failed: %s" % message
		if ok:
			Compat.rescan(_plugin)
			changed.emit()
			reload()
			return
		var again: Variant = pressed.get_ref()
		if again != null:
			again.disabled = false
			again.text = "Update"
	)


func _ask_removal(row: Dictionary) -> void:
	_pending_removal = row
	var res_path := String(row.get("res_path", ""))
	_confirm.dialog_text = "Delete %s from this project?\n\nThe library keeps its copy — this only removes the file this project imported, and the record of the import." % res_path
	_confirm.popup_centered()


func _do_removal() -> void:
	var row := _pending_removal
	_pending_removal = {}
	var res_path := String(row.get("res_path", ""))
	var asset_id := int(row.get("asset_id", 0))

	# The project's own copy, never the library's file (invariant 1). res:// paths cannot
	# escape the project, and this one came from the manifest rather than from typing.
	if res_path.begins_with("res://") and FileAccess.file_exists(res_path):
		var err := DirAccess.remove_absolute(ProjectSettings.globalize_path(res_path))
		if err != OK:
			_status.text = "Could not delete %s (error %d)" % [res_path, err]
			return

	Project.forget(asset_id)
	Compat.rescan(_plugin)
	changed.emit()

	var use_id := int(row.get("use_id", 0))
	if use_id > 0 and Config.configured():
		_api().remove_use(Project.uuid(), use_id, func(ok, message):
			_status.text = "Removed %s" % res_path if ok else \
				"Removed it here; the server still lists it (%s)" % message
			reload()
		)
	else:
		_status.text = "Removed %s" % res_path
		reload()


func _write_credits() -> void:
	if not Config.configured():
		_status.text = "Not connected."
		return
	_status.text = "Fetching credits…"
	var api: RefCounted = _api()
	var url := Config.base_url() + "/api/v1/projects/" + Project.uuid().uri_encode() + "/credits.md"
	api.fetch_bytes(url, func(ok, bytes):
		if not ok:
			_status.text = "Could not fetch the credits file — is this project known to the server yet?"
			return
		var f := FileAccess.open("res://CREDITS.md", FileAccess.WRITE)
		if f == null:
			_status.text = "Could not write res://CREDITS.md"
			return
		f.store_buffer(bytes)
		f.close()
		Compat.rescan(_plugin)
		_status.text = "Wrote res://CREDITS.md"
	)


static func _bytes(n: int) -> String:
	if n < 1024:
		return "%d B" % n
	if n < 1024 * 1024:
		return "%.1f KB" % (n / 1024.0)
	if n < 1024 * 1024 * 1024:
		return "%.1f MB" % (n / 1048576.0)
	return "%.2f GB" % (n / 1073741824.0)
