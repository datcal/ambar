extends SceneTree
## Drives the "In this project" screen.
##
## Everything this screen says is a comparison between two things that drift apart in real use —
## the committed manifest and the server's record — so the test drives them apart deliberately and
## checks the screen notices: an import the server was never told about, an asset the library has
## moved on from, and a file removed from the project.
##
##   godot --script test_project.gd --path <project>
##
## Needs a running Ambar. Leaves the test project's manifest and res://assets empty.

const Main := preload("res://addons/ambar/main.gd")
const Project := preload("res://addons/ambar/project.gd")
const Api := preload("res://addons/ambar/api.gd")
const Config := preload("res://addons/ambar/config.gd")

var _failures := 0
var _host: Node
var _main: Control


func _initialize() -> void:
	_host = Node.new()
	root.add_child(_host)
	_go()


func check(label: String, ok: bool, detail: String = "") -> void:
	if ok:
		print("  ok    %s%s" % [label, ("  " + detail) if detail != "" else ""])
	else:
		_failures += 1
		print("  FAIL  %s  %s" % [label, detail])


func _api() -> RefCounted:
	return Api.new(Config.base_url(), Config.token(), _host)


func await_call(method: Callable) -> Array:
	var out: Array = []
	method.call(func(ok, result): out.append([ok, result]))
	var frames := 0
	while out.is_empty():
		await process_frame
		frames += 1
		if frames > 3000:
			return [false, "no answer from the server"]
	return out[0]


func _until(condition: Callable, seconds: int) -> bool:
	var until := Time.get_ticks_msec() + seconds * 1000
	while Time.get_ticks_msec() < until:
		if condition.call():
			return true
		await process_frame
	return false


## _rows is what the screen is currently showing, as (filename, badge) pairs.
func _rows() -> Array:
	var out: Array = []
	for line in _main._project._rows.get_children():
		var labels: Array = []
		for child in line.get_children():
			if child is Label:
				labels.append(child.text)
			elif child is VBoxContainer:
				for sub in child.get_children():
					if sub is Label:
						labels.append(sub.text)
		if labels.size() >= 3:
			out.append([labels[0], labels[2]])
	return out


func _badge_for(filename: String) -> String:
	for row in _rows():
		if String(row[0]) == filename:
			return String(row[1])
	return "<no row>"


func _go() -> void:
	await process_frame
	print("ambar plugin — the project screen against %s" % Config.base_url())

	# Start from nothing, so the counts in this test mean what they say — on *both* sides. The
	# import pass runs before this one and leaves a use row under the same project uuid, which
	# made "the server lists both" report three.
	_wipe()
	await _wipe_server()

	root.size = Vector2i(1500, 900)
	_main = Main.new()
	root.add_child(_main)
	# Without this the control is 0×0 and every container inside it collapses to its minimum —
	# the screen renders, the tests pass, and the screenshot is a header over nothing. plugin.gd
	# does the same when it parents the panel to the editor's main screen.
	_main.set_anchors_and_offsets_preset(Control.PRESET_FULL_RECT)
	_main.set_plugin(null)
	await process_frame
	_main._kind.select(0)
	_main._search.text = "hare"
	_main._do_search(true)
	await _until(func(): return not _main._grid._tiles.is_empty(), 30)

	var tiles: Array = _main._grid._flow.get_children()
	check("library found something to import", tiles.size() >= 2, "%d tiles" % tiles.size())
	if tiles.size() < 2:
		quit(1)
		return

	# --- import two, the ordinary way ------------------------------------------------
	var first: Dictionary = tiles[0].get_meta("asset")
	var second: Dictionary = tiles[1].get_meta("asset")
	_main._on_import_requested(first)
	await _until(func(): return Project.manifest().has(str(int(first.get("id", 0)))), 30)
	_main._on_import_requested(second)
	await _until(func(): return Project.manifest().has(str(int(second.get("id", 0)))), 30)
	check("two imports in the manifest", Project.manifest().size() == 2,
		"%d entries" % Project.manifest().size())

	_main._tabs.set_current_tab(1)
	await _until(func(): return _rows().size() == 2, 30)
	check("the screen lists them", _rows().size() == 2, str(_rows()))
	check("nothing is wrong with them", _badge_for(String(first.get("filename", ""))) == "",
		"badge = '%s'" % _badge_for(String(first.get("filename", ""))))
	# The tab count follows the import callback, which lands after the manifest write the loop
	# above waited on — so wait for it rather than racing it.
	await _until(func(): return _main._tabs.get_tab_title(1).contains("(2)"), 20)
	check("the tab counts them", _main._tabs.get_tab_title(1).contains("(2)"),
		_main._tabs.get_tab_title(1))

	# --- an import the server never heard about --------------------------------------
	#
	# The offline case the specification promises is replayable: drop the server's row, keep the manifest.
	# record_use is fired after the manifest write, so give it a moment to land rather than
	# asserting against a request that may still be in flight.
	var rows: Array = []
	await _until(func(): return true, 1)
	for attempt in 20:
		var uses: Array = await await_call(func(cb): _api().project_uses(Project.uuid(), cb))
		rows = (uses[1] as Dictionary).get("uses", []) if uses[0] and uses[1] is Dictionary else []
		if rows.size() >= 2:
			break
		await _until(func(): return false, 1)
	check("the server lists both", rows.size() == 2, "%d uses" % rows.size())

	var victim: Dictionary = {}
	for raw in rows:
		if int(raw.get("asset_id", 0)) == int(second.get("id", 0)):
			victim = raw
	var dropped: Array = await await_call(func(cb): _api().remove_use(Project.uuid(), int(victim.get("id", 0)), cb))
	check("dropped one server row", dropped[0], str(dropped[1]))

	_main._project.reload()
	await _until(func(): return _badge_for(String(second.get("filename", ""))) == "not recorded on the server", 30)
	check("the screen notices", _badge_for(String(second.get("filename", ""))) == "not recorded on the server",
		_badge_for(String(second.get("filename", ""))))
	check("and offers to fix it", _main._project._sync_button.text.contains("Sync 1"),
		_main._project._sync_button.text)

	_main._project._sync()
	await _until(func(): return _badge_for(String(second.get("filename", ""))) == "", 30)
	check("sync replays it", _badge_for(String(second.get("filename", ""))) == "",
		_badge_for(String(second.get("filename", ""))))

	# --- an asset the library has moved on from --------------------------------------
	#
	# Recording the use with a hash that is not the library's current one is exactly what an
	# import made before the file changed looks like from the server's side.
	var stale: Array = await await_call(func(cb): _api().record_use(Project.uuid(), Project.name_hint(),
		int(first.get("id", 0)), String(Project.manifest()[str(int(first.get("id", 0)))]["res_path"]),
		"a-hash-from-before", cb))
	check("recorded a stale hash", stale[0], str(stale[1]))

	_main._project.reload()
	await _until(func(): return _badge_for(String(first.get("filename", ""))) == "library has a newer version", 30)
	check("the screen calls it outdated",
		_badge_for(String(first.get("filename", ""))) == "library has a newer version",
		_badge_for(String(first.get("filename", ""))))

	# A picture of the screen in its most interesting state, for a human to look at. Only when
	# asked for and only with a display: headless has no renderer, and reading a viewport texture
	# there fails with "Parameter t is null" and then wedges the run.
	if OS.get_cmdline_user_args().size() > 0 and DisplayServer.get_name() != "headless":
		await _until(func(): return false, 3)
		var shot_path := OS.get_cmdline_user_args()[0]
		var shot := root.get_texture().get_image()
		print("  ..    screenshot %s (error %d)" % [shot_path, shot.save_png(shot_path)])
	# Update re-downloads over the same res:// path and tells the server the new hash.
	var update_button: Button = _find_button("Update")
	check("there is an Update button", update_button != null)
	if update_button != null:
		update_button.emit_signal("pressed")
		await _until(func(): return _badge_for(String(first.get("filename", ""))) == "", 40)
		check("update clears it", _badge_for(String(first.get("filename", ""))) == "",
			_badge_for(String(first.get("filename", ""))))

	# --- a file deleted from the project ---------------------------------------------
	var res_path := String(Project.manifest()[str(int(second.get("id", 0)))]["res_path"])
	DirAccess.remove_absolute(ProjectSettings.globalize_path(res_path))
	_main._project.reload()
	await _until(func(): return _badge_for(String(second.get("filename", ""))) == "missing from this project", 30)
	check("a deleted file is reported", _badge_for(String(second.get("filename", ""))) == "missing from this project",
		_badge_for(String(second.get("filename", ""))))

	# --- removing an import ------------------------------------------------------------
	var before := Project.manifest().size()
	# The await is on its own line: inside a dictionary literal it evaluated to nothing useful and
	# the use id came out 0, which the server accepts as a no-op delete.
	var current: Array = await await_call(func(cb): _api().project_uses(Project.uuid(), cb))
	var first_use := _use_id_of(current, int(first.get("id", 0)))
	check("found the use row to remove", first_use > 0, "use id %d" % first_use)
	_main._project._pending_removal = {
		"asset_id": int(first.get("id", 0)),
		"res_path": String(Project.manifest()[str(int(first.get("id", 0)))]["res_path"]),
		"use_id": first_use,
	}
	var removed_path := String(_main._project._pending_removal["res_path"])
	_main._project._do_removal()
	await _until(func(): return Project.manifest().size() == before - 1, 30)

	check("the file is gone from the project", not FileAccess.file_exists(removed_path), removed_path)
	check("the manifest forgot it", not Project.manifest().has(str(int(first.get("id", 0)))),
		"%d entries left" % Project.manifest().size())

	# The DELETE is fired after the local removal the loop above waited on, so poll for it.
	var left: Array = []
	var still_there := true
	for attempt in 20:
		var after: Array = await await_call(func(cb): _api().project_uses(Project.uuid(), cb))
		left = (after[1] as Dictionary).get("uses", []) if after[0] and after[1] is Dictionary else []
		still_there = false
		for raw in left:
			if int(raw.get("asset_id", 0)) == int(first.get("id", 0)):
				still_there = true
		if not still_there:
			break
		await _until(func(): return false, 1)
	check("the server released it too", not still_there, "%d uses left" % left.size())

	# The library still has it — removing from a project must never touch the library.
	var lookup: Array = await await_call(func(cb): _api().asset(int(first.get("id", 0)), cb))
	check("the library is untouched", lookup[0],
		String((lookup[1] as Dictionary).get("asset", {}).get("filename", "")) if lookup[0] else str(lookup[1]))

	print("%s (%d failure%s)" % ["FAILED" if _failures > 0 else "PASSED", _failures, "" if _failures == 1 else "s"])
	quit(1 if _failures > 0 else 0)


func _find_button(text: String) -> Button:
	for line in _main._project._rows.get_children():
		for child in line.get_children():
			if child is Button and child.text == text:
				return child
	return null


func _use_id_of(response: Array, asset_id: int) -> int:
	if not response[0] or not response[1] is Dictionary:
		return 0
	for raw in ((response[1] as Dictionary).get("uses", []) as Array):
		if int(raw.get("asset_id", 0)) == asset_id:
			return int(raw.get("id", 0))
	return 0


## _wipe_server drops this project's recorded uses, so the harness starts from an empty ledger.
## Only ever *this* project's rows, and never anything in the library.
func _wipe_server() -> void:
	var listed: Array = await await_call(func(cb): _api().project_uses(Project.uuid(), cb))
	if not listed[0] or not listed[1] is Dictionary:
		return
	for raw in ((listed[1] as Dictionary).get("uses", []) as Array):
		await await_call(func(cb): _api().remove_use(Project.uuid(), int(raw.get("id", 0)), cb))


## _wipe clears the test project's imports so the counts are deterministic. Only ever the test
## project's own res:// tree — nothing here can reach the library.
func _wipe() -> void:
	var manifest := Project.manifest()
	for key in manifest:
		var entry: Dictionary = manifest[key] if manifest[key] is Dictionary else {}
		var path := String(entry.get("res_path", ""))
		if path.begins_with("res://") and FileAccess.file_exists(path):
			DirAccess.remove_absolute(ProjectSettings.globalize_path(path))
	DirAccess.remove_absolute(ProjectSettings.globalize_path(Project.MANIFEST_FILE))
