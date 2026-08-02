extends SceneTree
## Renders the plugin's main screen in a real window and saves a screenshot.
##
## The headless drive proves the API client works; this proves the *panel* does — that the grid
## reflows, that the pager renders, that the inspector fills in. It runs outside the editor, so the
## theme is Godot's default rather than the editor's, but every control, layout and request is the
## same code path the editor tab uses.
##
##   godot --script test_ui.gd --path <project> -- <query> <shot.png>

const Main := preload("res://addons/ambar/main.gd")

var _main: Control


func _initialize() -> void:
	_go()


func _go() -> void:
	await process_frame

	var args := OS.get_cmdline_user_args()
	var query := args[0] if args.size() > 0 else ""
	var shot := args[1] if args.size() > 1 else "user://ambar_ui.png"
	var width := int(args[2]) if args.size() > 2 else 1600
	var height := int(args[3]) if args.size() > 3 else 950

	root.size = Vector2i(width, height)
	root.title = "Ambar plugin — UI harness"

	_main = Main.new()
	root.add_child(_main)
	_main.set_anchors_and_offsets_preset(Control.PRESET_FULL_RECT)
	# No EditorPlugin outside the editor. set_plugin builds the UI either way; the plugin handle is
	# only needed to rescan the editor filesystem after an import, which this harness does not do.
	_main.set_plugin(null)

	await process_frame
	_main._load_sorts()
	if query != "":
		_main._search.text = query
	_main._do_search(true)

	# Wait for the page itself, then let the pictures fill in.
	#
	# Waiting on a *condition*, not on a duration. Neither clock works alone here: an HTTPRequest
	# is polled once per frame, and an unfocused window is throttled by the compositor, so five
	# wall-clock seconds can be a handful of frames and a screenshot of "Searching…". A frame
	# count has the opposite failure — 2400 frames was forty seconds on one run and four minutes
	# on another.
	await _until(func(): return not _main._grid._tiles.is_empty(), 60)
	await _settle(int(args[4]) if args.size() > 4 else 5)

	# Open a tile in the inspector, so the screenshot shows the panel doing its job. A
	# multi-format one when the page has one, because the variant picker is the part worth seeing.
	var tiles: Array = _main._grid._flow.get_children()
	var chosen: Node = null
	for tile in tiles:
		var a: Dictionary = tile.get_meta("asset")
		if int(a.get("variant_count", 1)) > 1:
			chosen = tile
			break
	if chosen == null and not tiles.is_empty():
		chosen = tiles[0]
	if chosen != null:
		chosen.emit_signal("pressed")
		# Wait for the panel's own picture, which for a model means fetching a preview.glb and
		# rendering it here. Same reason as above: a fixed wait screenshots "Loading…".
		await _until(func(): return _main._detail._preview.texture != null, 60)
		await _settle(int(args[5]) if args.size() > 5 else 2)

	var image := root.get_texture().get_image()
	var err := image.save_png(shot)
	print("tiles=%d  saved=%s (error %d)" % [tiles.size(), shot, err])
	quit(0)


## _settle waits a number of seconds, in frames, so the page has time to arrive and draw.
func _settle(seconds: int) -> void:
	var until := Time.get_ticks_msec() + seconds * 1000
	while Time.get_ticks_msec() < until:
		await process_frame


## _until waits for a condition, giving up after a number of seconds so a broken server fails the
## run instead of hanging it.
func _until(condition: Callable, seconds: int) -> bool:
	var until := Time.get_ticks_msec() + seconds * 1000
	while Time.get_ticks_msec() < until:
		if condition.call():
			return true
		await process_frame
	return false
