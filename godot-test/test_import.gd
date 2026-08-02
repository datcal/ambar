extends SceneTree
## Imports through the panel, the way a person does: search, select, press the button.
##
## the design's import path is the one place the plugin writes to the project and to the server, so it is
## the one worth driving end to end rather than unit-testing the pieces. It checks the file landed,
## the manifest recorded it, and the panel's own state caught up.
##
##   godot --script test_import.gd --path <project> -- <query>

const Main := preload("res://addons/ambar/main.gd")
const Project := preload("res://addons/ambar/project.gd")
const Importer := preload("res://addons/ambar/importer.gd")

var _failures := 0


func _initialize() -> void:
	_go()


func check(label: String, ok: bool, detail: String = "") -> void:
	if ok:
		print("  ok    %s%s" % [label, ("  " + detail) if detail != "" else ""])
	else:
		_failures += 1
		print("  FAIL  %s  %s" % [label, detail])


func _go() -> void:
	await process_frame

	var args := OS.get_cmdline_user_args()
	var query := args[0] if args.size() > 0 else "hare"

	root.size = Vector2i(1400, 900)
	var main: Control = Main.new()
	root.add_child(main)
	main.set_plugin(null)
	await process_frame

	# The panel remembers a kind filter per person, and a harness that inherits it searches
	# whatever the last session was looking at. Force the neutral one.
	main._kind.select(0)
	main._search.text = query
	main._do_search(true)
	for i in 200:
		await process_frame

	var tiles: Array = main._grid._flow.get_children()
	check("search found tiles", not tiles.is_empty(), "%d tiles" % tiles.size())
	if tiles.is_empty():
		quit(1)
		return

	var tile: Node = tiles[0]
	var asset: Dictionary = tile.get_meta("asset")
	tile.emit_signal("pressed")
	for i in 120:
		await process_frame

	check("inspector opened", main._detail._current.get("id", 0) == asset.get("id", 0),
		String(asset.get("filename", "")))

	# Press Import exactly as the panel's own button does.
	main._detail.import_requested.emit(main._detail._current)
	for i in 400:
		await process_frame

	var asset_id := int(asset.get("id", 0))
	var manifest: Dictionary = Project.manifest()
	check("manifest recorded it", manifest.has(str(asset_id)), str(manifest.get(str(asset_id), {})))

	var entry: Dictionary = manifest.get(str(asset_id), {})
	var res_path := String(entry.get("res_path", ""))
	check("file is in the project", res_path != "" and FileAccess.file_exists(res_path), res_path)
	if res_path != "":
		var f := FileAccess.open(res_path, FileAccess.READ)
		check("file has bytes", f != null and f.get_length() > 0,
			"%d bytes" % (f.get_length() if f != null else 0))

	check("panel says so", main._detail._import_button.text == "In project",
		main._detail._import_button.text)
	check("tile says so", String(main._grid._tiles[asset_id].get_meta("badges").text).contains("✓"),
		String(main._grid._tiles[asset_id].get_meta("badges").text))
	check("status reported", main._grid._status.text.contains("Imported"), main._grid._status.text)

	# --- content that is not what was advertised ------------------------------------
	#
	# The import must refuse rather than record provenance for bytes nobody verified. This is
	# the check that would have caught a project ending up with `3.png` from one pack whose
	# content was a file from another, and a CREDITS.md that faithfully credited the wrong one.
	var lie: Dictionary = asset.duplicate()
	lie["sha256"] = "0000000000000000000000000000000000000000000000000000000000000000"
	lie["filename"] = "lied_about.png"
	var refused: Array = []
	Importer.import_asset(main._api(), lie, func(ok, message, res):
		refused.append([ok, message, res]))
	for i in 600:
		if not refused.is_empty():
			break
		await process_frame
	var outcome: Array = refused[0] if not refused.is_empty() else [true, "no answer", ""]
	check("a content mismatch is refused", not outcome[0], String(outcome[1]).substr(0, 90))
	check("and nothing is left behind", not Project.manifest().has(str(int(lie.get("id", 0)) )) or
		String(Project.manifest().get(str(int(lie.get("id", 0))), {}).get("filename", "")) != "lied_about.png",
		"manifest has %d entries" % Project.manifest().size())

	print("%s (%d failure%s)" % ["FAILED" if _failures > 0 else "PASSED", _failures, "" if _failures == 1 else "s"])
	quit(1 if _failures > 0 else 0)
