@tool
extends VBoxContainer
## The results area: a grid that reflows to the window, a numbered pager, and a thumbnail loader
## that keeps a handful of requests in flight rather than a hundred.
##
## What it replaces, and why each part had to go:
##
##   * `GridContainer` with `columns = 6`. Six is the right number for exactly one window width.
##     `HFlowContainer` wraps at whatever fits, so a wide editor shows a wide grid — which is the
##     complaint this milestone started from.
##   * A fixed 96-pixel thumbnail. Pixel art at 96px and a 4K character sheet at 96px are not the
##     same browsing problem, so the size is a control, and it is remembered.
##   * "Load more". A cursor can only go forward: no page 3, no way back, no idea how far in you
##     are. The server pages by number now and hands over the page links it already computes.
##
## The thumbnail queue is not premature: one page is up to 120 tiles, the target deployment is a
## Synology NAS, and 120 simultaneous HTTPRequest nodes each holding a connection is a way to make
## a library feel broken that has nothing to do with the library.

signal selected(asset: Dictionary)
signal activated(asset: Dictionary)
signal page_requested(page: int)

const MAX_IN_FLIGHT := 6

var _scroll: ScrollContainer
var _flow: HFlowContainer
var _pager: HBoxContainer
var _status: Label

var _api_factory: Callable
var _tile_size := 128
var _imported: Dictionary = {}
var _selected_id := 0
var _tiles: Dictionary = {} # asset id (int) → the tile Button

# Pending (asset_id, TextureRect) pairs and how many are being fetched right now.
var _queue: Array = []
var _in_flight := 0

# Models with no thumbnail on the server, waiting to be drawn here. One at a time: there is one
# viewport and one camera, and a page of models is background work, not a race.
var _renderer: Node
var _render_queue: Array = []
var _rendering := false


func setup(api_factory: Callable, tile_size: int, renderer: Node = null) -> void:
	_api_factory = api_factory
	_tile_size = tile_size
	_renderer = renderer
	size_flags_vertical = Control.SIZE_EXPAND_FILL
	# The grid takes the space the inspector does not; without this the split container sizes it
	# to one tile and the whole window becomes an inspector.
	size_flags_horizontal = Control.SIZE_EXPAND_FILL

	_scroll = ScrollContainer.new()
	_scroll.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_scroll.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_scroll.horizontal_scroll_mode = ScrollContainer.SCROLL_MODE_DISABLED
	add_child(_scroll)

	_flow = HFlowContainer.new()
	_flow.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_scroll.add_child(_flow)

	_pager = HBoxContainer.new()
	_pager.alignment = BoxContainer.ALIGNMENT_CENTER
	add_child(_pager)

	_status = Label.new()
	_status.horizontal_alignment = HORIZONTAL_ALIGNMENT_CENTER
	_status.modulate = Color(1, 1, 1, 0.7)
	add_child(_status)


## set_tile_size re-lays the current page at a new size without going back to the server for the
## page itself — only, when the size crosses into 2x territory, for sharper pictures.
func set_tile_size(px: int) -> void:
	if px == _tile_size:
		return
	var was_large := _wants_2x()
	_tile_size = px
	for id in _tiles:
		var tile: Control = _tiles[id]
		tile.custom_minimum_size = _tile_extent()
		var picture: TextureRect = tile.get_meta("picture")
		picture.custom_minimum_size = Vector2(_tile_size, _tile_size)

	# Choosing 256px to see something properly and getting the 256px thumbnail stretched to fill
	# it would defeat the point of the control. Only when the threshold is actually crossed, so
	# stepping 64 → 96 → 128 costs nothing.
	if _wants_2x() != was_large:
		_queue.clear()
		for id in _tiles:
			_queue.append([int(id), _tiles[id].get_meta("picture")])
		_pump()


## set_imported updates the "in project" badges from the manifest.
func set_imported(imported: Dictionary) -> void:
	_imported = imported
	for id in _tiles:
		_mark_tile(_tiles[id], int(id))


func status(text: String) -> void:
	_status.text = text


func clear() -> void:
	_queue.clear()
	_render_queue.clear()
	_tiles.clear()
	# Detached before freeing: queue_free defers to the end of the frame, so tiles from the page
	# being replaced would still be children while the new page is added to the same container.
	for child in _flow.get_children():
		_flow.remove_child(child)
		child.queue_free()
	for child in _pager.get_children():
		_pager.remove_child(child)
		child.queue_free()
	_scroll.scroll_vertical = 0


## show_page renders one search response: the tiles, then the pager beneath them.
func show_page(result: Dictionary) -> void:
	clear()

	var rows: Array = result.get("assets", [])
	for row in rows:
		if row is Dictionary:
			_add_tile(row)

	_build_pager(result)

	var total := int(result.get("total", rows.size()))
	if total == 0:
		_status.text = "Nothing matches that."
	else:
		_status.text = "%d–%d of %d" % [
			int(result.get("first_shown", 1)),
			int(result.get("last_shown", rows.size())),
			total,
		]

	_pump()


# --- tiles ---------------------------------------------------------------------------

func _tile_extent() -> Vector2:
	# Room under the picture for two lines of filename *and* the badge line. Too tight and the
	# badge renders past the bottom of the tile, which reads as a rendering bug rather than a
	# badge.
	return Vector2(_tile_size + 16, _tile_size + 76)


func _add_tile(a: Dictionary) -> void:
	var id := int(a.get("id", 0))
	var filename := String(a.get("filename", "asset"))

	var tile := Button.new()
	tile.toggle_mode = true
	tile.custom_minimum_size = _tile_extent()
	tile.tooltip_text = _tooltip(a)
	tile.set_meta("asset", a)

	var box := VBoxContainer.new()
	box.set_anchors_and_offsets_preset(Control.PRESET_FULL_RECT, Control.PRESET_MODE_KEEP_SIZE, 4)
	# The contents are decoration; the Button underneath is what is clicked.
	box.mouse_filter = Control.MOUSE_FILTER_IGNORE
	tile.add_child(box)

	var picture := TextureRect.new()
	picture.custom_minimum_size = Vector2(_tile_size, _tile_size)
	picture.size_flags_vertical = Control.SIZE_EXPAND_FILL
	picture.expand_mode = TextureRect.EXPAND_IGNORE_SIZE
	picture.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED
	# Pixel art is the common case in this library, and a smoothed 32px sprite in a 128px box is
	# unreadable — the same lesson the web viewer had to learn.
	picture.texture_filter = CanvasItem.TEXTURE_FILTER_NEAREST
	picture.mouse_filter = Control.MOUSE_FILTER_IGNORE
	box.add_child(picture)

	var name_label := Label.new()
	name_label.text = filename
	# Two lines, wrapped, ellipsis on the second.
	#
	# One line cannot hold "Swordsman_lvl1_Walk_with_shadow.png" at any tile size somebody would
	# actually browse at, and every way of shortening it to one line was tried and was worse:
	# trailing ellipsis gives a page of "Swordsman_lvl…", leading gives a page of "…aseprite", and
	# middle gives a page of "Sword…dow.png" — these names differ in the middle *and* at both ends
	# depending on the pack. Two lines of 128 pixels hold most of them outright.
	name_label.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	name_label.max_lines_visible = 2
	name_label.text_overrun_behavior = TextServer.OVERRUN_TRIM_ELLIPSIS
	name_label.mouse_filter = Control.MOUSE_FILTER_IGNORE
	box.add_child(name_label)

	var badges := Label.new()
	badges.horizontal_alignment = HORIZONTAL_ALIGNMENT_CENTER
	badges.modulate = Color(1, 1, 1, 0.7)
	badges.mouse_filter = Control.MOUSE_FILTER_IGNORE
	box.add_child(badges)

	tile.set_meta("picture", picture)
	tile.set_meta("badges", badges)
	tile.pressed.connect(func(): _select(id))
	tile.gui_input.connect(func(event: InputEvent): _on_tile_input(event, a))

	_flow.add_child(tile)
	_tiles[id] = tile
	_mark_tile(tile, id)
	_queue.append([id, picture])


func _tooltip(a: Dictionary) -> String:
	var parts: Array = [String(a.get("filename", "")), String(a.get("kind", ""))]
	var w := int(a.get("width", 0))
	var h := int(a.get("height", 0))
	if w > 0 and h > 0:
		parts.append("%d×%d" % [w, h])
	var pack: Dictionary = a.get("pack", {}) if a.get("pack") is Dictionary else {}
	if String(pack.get("name", "")) != "":
		parts.append(String(pack.get("name")))
	return " · ".join(PackedStringArray(parts))


## _mark_tile writes the badge line: how many formats this asset has, and whether the project
## already contains it (the specification asks for both).
func _mark_tile(tile: Button, id: int) -> void:
	var a: Dictionary = tile.get_meta("asset")
	var badges: Label = tile.get_meta("badges")
	var text := ""
	var variants := int(a.get("variant_count", 1))
	if variants > 1:
		text = "%d formats" % variants
	if _imported.has(str(id)):
		text = ("✓ in project" if text == "" else "✓ · " + text)
	badges.text = text


func _select(id: int) -> void:
	_selected_id = id
	for other_id in _tiles:
		var tile: Button = _tiles[other_id]
		tile.set_pressed_no_signal(int(other_id) == id)
	if _tiles.has(id):
		selected.emit(_tiles[id].get_meta("asset"))


func _on_tile_input(event: InputEvent, a: Dictionary) -> void:
	if event is InputEventMouseButton and event.double_click and event.button_index == MOUSE_BUTTON_LEFT:
		activated.emit(a)


# --- thumbnails ----------------------------------------------------------------------

## _pump keeps at most MAX_IN_FLIGHT thumbnail requests going. Tiles load top-down because the
## queue is filled in grid order, which is the order somebody is looking at them in.
func _pump() -> void:
	while _in_flight < MAX_IN_FLIGHT and not _queue.is_empty():
		var job: Array = _queue.pop_front()
		_fetch_thumb(int(job[0]), job[1])


## _wants_2x reports whether tiles are big enough to need the 1024px thumbnail. Below the
## threshold the 256px one is already more than a tile can show.
func _wants_2x() -> bool:
	return _tile_size > 192


func _fetch_thumb(asset_id: int, into: TextureRect) -> void:
	if not is_instance_valid(into):
		_pump()
		return
	_in_flight += 1
	var api: RefCounted = _api_factory.call()
	var url: String = api.thumb2x_url(asset_id) if _wants_2x() else api.thumb_url(asset_id)
	# A weak reference, not the node. Changing page frees every tile, and a lambda that has
	# captured a freed object is not merely useless — Godot refuses to call it at all and logs
	# "Lambda capture at index 0 was freed", so the `is_instance_valid` guard inside never runs
	# and neither does anything else in the callback, including the bookkeeping.
	var target := weakref(into)
	api.fetch_bytes(url, func(ok, bytes):
		_in_flight -= 1
		var tile: Variant = target.get_ref()
		var texture := _decode(bytes) if ok else null
		if texture != null:
			if tile != null:
				tile.texture = texture
		elif tile != null:
			# No picture on the server. For a model that is the normal case rather than an
			# error — nothing has rendered it yet — and this plugin can.
			_queue_render(asset_id, tile)
		_pump()
	)


## _decode turns thumbnail bytes into a texture. Thumbnails are WebP; the PNG fallback covers
## a server that ever serves something else.
static func _decode(bytes: PackedByteArray) -> ImageTexture:
	if bytes.is_empty():
		return null
	var image := Image.new()
	var err := image.load_webp_from_buffer(bytes)
	if err != OK:
		err = image.load_png_from_buffer(bytes)
	if err != OK:
		return null
	return ImageTexture.create_from_image(image)


# --- rendering the models nobody has drawn yet ----------------------------------------
#
# the specification keeps Blender optional and the server has no renderer, so a model's derive produces a
# normalised preview.glb and never a picture. The web viewer fills thumbnails in as people open
# assets; everything nobody has opened is a blank tile. This plugin runs inside a renderer,
# so it draws the ones it meets and posts them back — see model_render.gd.

func _queue_render(asset_id: int, into: TextureRect) -> void:
	if _renderer == null or not _tiles.has(asset_id):
		return
	var a: Dictionary = _tiles[asset_id].get_meta("asset")
	var links: Dictionary = a.get("links", {}) if a.get("links") is Dictionary else {}
	# preview_glb is only advertised for a model derive could normalise. An .fbx in a
	# Blender-less deployment has none, and there is nothing here that can read one.
	if not links.has("preview_glb"):
		return
	_render_queue.append([asset_id, into])
	_pump_renders()


func _pump_renders() -> void:
	if _rendering or _render_queue.is_empty():
		return
	var job: Array = _render_queue.pop_front()
	_start_render(int(job[0]), job[1])


func _start_render(asset_id: int, into: TextureRect) -> void:
	_rendering = true
	var api: RefCounted = _api_factory.call()
	var target := weakref(into)
	api.fetch_bytes(api.preview_glb_url(asset_id), func(ok, bytes):
		if not ok:
			_rendering = false
			_pump_renders()
			return
		_draw_model(asset_id, target, bytes)
	)


func _draw_model(asset_id: int, target: WeakRef, glb: PackedByteArray) -> void:
	var image: Image = await _renderer.render(glb)
	if image != null:
		var tile: Variant = target.get_ref()
		if tile != null:
			tile.texture = ImageTexture.create_from_image(image)
		# Stored even when the tile it was for has gone: the render already cost the time, and
		# the next person to browse here should not pay it again.
		_store_render(asset_id, image)
	_rendering = false
	_pump_renders()


## _store_render hands the picture to the server, so the next person to look — here or in the
## browser — is served a thumbnail instead of drawing it again.
func _store_render(asset_id: int, image: Image) -> void:
	var api: RefCounted = _api_factory.call()
	api.upload_thumb(asset_id, image.save_png_to_buffer(), func(ok, message):
		if not ok:
			# Not worth interrupting a browse over: the picture is on screen either way, and
			# the next viewer simply renders it again.
			push_warning("Ambar: rendered asset %d but could not store it — %s" % [asset_id, message])
	)


# --- pager ---------------------------------------------------------------------------

## _build_pager renders "‹ 1 2 [3] 4 … 57 ›" from what the server sent. The window of links is
## computed server-side, where the web grid's version already lives; a 0 stands for a gap.
func _build_pager(result: Dictionary) -> void:
	var page := int(result.get("page", 1))
	var pages := int(result.get("pages", 1))
	if pages <= 1:
		return

	var prev := Button.new()
	prev.text = "‹"
	prev.tooltip_text = "Previous page"
	prev.disabled = page <= 1
	prev.pressed.connect(func(): page_requested.emit(page - 1))
	_pager.add_child(prev)

	var numbers: Array = result.get("page_numbers", [])
	if numbers.is_empty():
		numbers = [page]
	for raw in numbers:
		var n := int(raw)
		if n == 0:
			var gap := Label.new()
			gap.text = "…"
			_pager.add_child(gap)
			continue
		var button := Button.new()
		button.text = str(n)
		button.toggle_mode = true
		button.set_pressed_no_signal(n == page)
		button.disabled = n == page
		button.pressed.connect(func(): page_requested.emit(n))
		_pager.add_child(button)

	var next := Button.new()
	next.text = "›"
	next.tooltip_text = "Next page"
	next.disabled = page >= pages
	next.pressed.connect(func(): page_requested.emit(page + 1))
	_pager.add_child(next)
