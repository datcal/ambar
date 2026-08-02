extends SceneTree
## Opening the tab must show the library. Nothing else.
##
## This exists because it did not, for a whole milestone, and no test noticed: every other harness
## calls `_do_search()` itself before checking anything, so all of them passed against a panel that
## opened empty and only filled in when somebody re-tested a connection that was already working.
##
## So this one does what a person does and nothing more: build the panel the way plugin.gd builds
## it — parent first, `set_plugin` second, exactly that order — and then wait.
##
##   godot --headless --script test_open.gd --path <project>

const Main := preload("res://addons/ambar/main.gd")
const Config := preload("res://addons/ambar/config.gd")

var _failures := 0


func _initialize() -> void:
	_go()


func check(label: String, ok: bool, detail: String = "") -> void:
	if ok:
		print("  ok    %s%s" % [label, ("  " + detail) if detail != "" else ""])
	else:
		_failures += 1
		print("  FAIL  %s  %s" % [label, detail])


func _until(condition: Callable, seconds: int) -> bool:
	var until := Time.get_ticks_msec() + seconds * 1000
	while Time.get_ticks_msec() < until:
		if condition.call():
			return true
		await process_frame
	return false


func _go() -> void:
	await process_frame
	print("ambar plugin — opening the tab against %s" % Config.base_url())

	check("configured", Config.configured(), Config.base_url())

	var main: Control = Main.new()
	main.name = "Ambar"
	main.hide()
	# plugin.gd's order, which is the whole point: the control is parented *before* it is told
	# about the plugin, because an HTTPRequest cannot run outside the tree. `_ready` therefore
	# fires with no plugin set, and anything that waited for one waited for ever.
	root.add_child(main)
	main.set_anchors_and_offsets_preset(Control.PRESET_FULL_RECT)
	main.set_plugin(null)
	main.show()

	# No _do_search, no _load_sorts, no clicking anything.
	var loaded := await _until(func(): return not main._grid._tiles.is_empty(), 45)
	check("the grid fills on its own", loaded, "%d tiles" % main._grid._tiles.size())
	check("the browse screen is the one showing", main._browse.visible and not main._setup.visible)
	check("the status line says what it found", main._grid._status.text.contains(" of "),
		main._grid._status.text)

	# The sort list is fetched by the same path, so a dropdown left saying "added" is the same bug
	# wearing a different hat.
	await _until(func(): return main._sort.get_item_count() > 1, 20)
	check("the sort dropdown is populated", main._sort.get_item_count() > 1,
		"%d orders" % main._sort.get_item_count())

	# Switching away and back must not re-query: on_shown loads once and then leaves it alone.
	var before: String = main._grid._status.text
	main.on_shown()
	await _until(func(): return false, 1)
	check("re-opening the tab does not re-query", main._grid._status.text == before,
		main._grid._status.text)

	print("%s (%d failure%s)" % ["FAILED" if _failures > 0 else "PASSED", _failures, "" if _failures == 1 else "s"])
	quit(1 if _failures > 0 else 0)
