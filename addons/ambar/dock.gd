@tool
extends VBoxContainer
## The Ambar dock (§10): search the library, and "Add to Project" an asset into
## res://assets/<kind>/<pack-slug>/<filename> with the right import preset,
## recording the import for credits and the "already imported" badge.
##
## Untested outside a Godot editor — this is the one component the Go suite
## cannot exercise (see docs/decisions.md). It is written to the §10 contract.

const Api := preload("res://addons/ambar/api.gd")
const Project := preload("res://addons/ambar/project.gd")

var _plugin: EditorPlugin
var _search: LineEdit
var _kind: OptionButton
var _results: ItemList
var _status: Label
var _assets: Array = [] # parallel to _results rows


func set_plugin(p: EditorPlugin) -> void:
	_plugin = p
	_build_ui()


func _build_ui() -> void:
	var bar := HBoxContainer.new()
	_search = LineEdit.new()
	_search.placeholder_text = "Search assets…"
	_search.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_search.text_submitted.connect(func(_t): _do_search())
	bar.add_child(_search)

	_kind = OptionButton.new()
	for k in ["any", "image", "model", "audio", "spritesheet"]:
		_kind.add_item(k)
	bar.add_child(_kind)

	var go := Button.new()
	go.text = "Search"
	go.pressed.connect(_do_search)
	bar.add_child(go)
	add_child(bar)

	_results = ItemList.new()
	_results.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_results.item_activated.connect(func(_i): _add_selected())
	add_child(_results)

	var actions := HBoxContainer.new()
	var add := Button.new()
	add.text = "Add to Project"
	add.pressed.connect(_add_selected)
	actions.add_child(add)

	var credits := Button.new()
	credits.text = "Generate CREDITS.md"
	credits.pressed.connect(_generate_credits)
	actions.add_child(credits)
	add_child(actions)

	_status = Label.new()
	_status.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	add_child(_status)


func _api() -> Api:
	return Api.new(_plugin.base_url(), _plugin.api_token(), get_tree())


func _do_search() -> void:
	if _plugin.api_token() == "":
		_status.text = "Set the Ambar base URL and API token in Editor Settings first."
		return
	_status.text = "Searching…"
	var kind := "" if _kind.get_item_text(_kind.selected) == "any" else _kind.get_item_text(_kind.selected)
	_api().search(_search.text, kind, func(ok, data):
		if not ok or not data is Dictionary:
			_status.text = "Search failed: %s" % str(data)
			return
		_populate(data.get("assets", []))
	)


func _populate(assets: Array) -> void:
	_assets = assets
	_results.clear()
	var imported := Project.manifest()
	for a in assets:
		var label := "%s  (%s)" % [a.get("filename", "?"), a.get("kind", "")]
		if imported.has(str(a.get("id", 0))):
			label += "  ✓ in project"
		_results.add_item(label)
	_status.text = "%d result(s)." % assets.size()


func _add_selected() -> void:
	var sel := _results.get_selected_items()
	if sel.is_empty():
		_status.text = "Select an asset first."
		return
	var a: Dictionary = _assets[sel[0]]
	_add_asset(a)


func _add_asset(a: Dictionary) -> void:
	var kind := String(a.get("kind", "other"))
	var filename := String(a.get("filename", "asset.bin"))
	# res://assets/<kind>/<pack-slug>/<filename> (§10). A real multi-file asset
	# would preserve relative structure; this imports the single primary file.
	var slug := String(a.get("pack", {}).get("slug", "pack"))
	var dir := "res://assets/%s/%s" % [kind, slug]
	DirAccess.make_dir_recursive_absolute(ProjectSettings.globalize_path(dir))
	var res_path := "%s/%s" % [dir, filename]
	var abs_dest := ProjectSettings.globalize_path(res_path)

	_status.text = "Downloading %s…" % filename
	_api().download_file(int(a.get("id", 0)), abs_dest, func(ok, _r):
		if not ok:
			_status.text = "Download failed."
			return
		_write_import_preset(res_path, a)
		EditorInterface.get_resource_filesystem().scan()
		_after_import(a, res_path)
	)


func _after_import(a: Dictionary, res_path: String) -> void:
	var asset_id := int(a.get("id", 0))
	Project.record(asset_id, {
		"res_path": res_path,
		"sha256": a.get("sha256", ""),
		"source_url": a.get("pack", {}).get("source_url", ""),
	})
	# Record to the server; tolerate being offline (§10: queue and retry). Here a
	# failure just leaves the manifest as the source of truth to reconcile later.
	_api().record_use(Project.uuid(), Project.name_hint(), asset_id, res_path, String(a.get("sha256", "")),
		func(ok, code):
			_status.text = "Added %s." % res_path if ok else "Added locally; server sync pending (%s)." % str(code)
	)


## _write_import_preset writes a .import next to the file before reimport (§10:
## pixel art nearest + no mipmaps; SFX no loop, music loop). The exact keys are
## Godot-version sensitive and must be verified in-editor.
func _write_import_preset(res_path: String, a: Dictionary) -> void:
	var import_path := ProjectSettings.globalize_path(res_path + ".import")
	var lines := PackedStringArray()
	match String(a.get("kind", "")):
		"image", "spritesheet":
			lines.append("[remap]")
			lines.append('importer="texture"')
			lines.append('type="CompressedTexture2D"')
			lines.append("[params]")
			lines.append("compress/mode=0") # lossless — never crunch pixel art
			if bool(a.get("is_pixel_art", false)):
				lines.append("mipmaps/generate=false")
				lines.append("process/fix_alpha_border=true")
		"audio":
			var importer := "wav" if res_path.ends_with(".wav") else "oggvorbisstr"
			lines.append("[remap]")
			lines.append('importer="%s"' % importer)
			lines.append("[params]")
			# Music loops, one-shots do not; a rough split is left to the human, so
			# default to no loop and let them flip it.
			lines.append("edit/loop_mode=0")
		_:
			return # models and others use Godot's defaults
	var f := FileAccess.open(import_path, FileAccess.WRITE)
	if f:
		f.store_string("\n".join(lines) + "\n")
		f.close()


func _generate_credits() -> void:
	_status.text = "Fetching credits…"
	var req := HTTPRequest.new()
	get_tree().root.add_child(req)
	req.request_completed.connect(func(result, code, _h, body):
		req.queue_free()
		if result != HTTPRequest.RESULT_SUCCESS or code != 200:
			_status.text = "Credits fetch failed (%d)." % code
			return
		var f := FileAccess.open("res://CREDITS.md", FileAccess.WRITE)
		if f:
			f.store_string(body.get_string_from_utf8())
			f.close()
			EditorInterface.get_resource_filesystem().scan()
			_status.text = "Wrote res://CREDITS.md"
	)
	var url := _plugin.base_url() + "/api/v1/projects/" + Project.uuid().uri_encode() + "/credits.md"
	req.request(url, PackedStringArray(["Authorization: Bearer " + _plugin.api_token()]))
