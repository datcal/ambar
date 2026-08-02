@tool
extends VBoxContainer
## The inspector panel: everything about one asset, without importing it first.
##
## The plugin used to offer a 96-pixel thumbnail, a filename and an Import button, so the only way
## to find out what a file actually was involved putting it in the project — and then deleting it
## again, which is exactly the kind of "did I already try this one" mess a library is supposed to
## prevent.
##
## One request fills this. `/api/v1/assets/{id}` returns the asset, its tags, its other formats and
## the pack's licence together, because the panel opens on every selection change and three round
## trips per click to a NAS is a panel that feels broken.

signal import_requested(asset: Dictionary)

const Api := preload("res://addons/ambar/api.gd")

var _api_factory: Callable
var _imported: Dictionary = {}
var _renderer: Node

var _title: Label
var _preview: TextureRect
var _preview_note: Label
var _facts: VBoxContainer
var _tags: RichTextLabel
var _variants_row: HBoxContainer
var _variants_box: VBoxContainer
var _import_button: Button
var _open_button: Button
var _empty: Label

# The asset the buttons act on — the selected variant, which is not always the primary.
var _current: Dictionary = {}
# Increments per selection so a slow response for the previous asset cannot overwrite the
# panel somebody is already looking at.
var _request := 0


func setup(api_factory: Callable, renderer: Node = null) -> void:
	_api_factory = api_factory
	_renderer = renderer
	size_flags_vertical = Control.SIZE_EXPAND_FILL
	custom_minimum_size.x = 260

	_empty = Label.new()
	_empty.text = "Select an asset to see it here.\nDouble-click imports it."
	_empty.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	_empty.modulate = Color(1, 1, 1, 0.6)
	add_child(_empty)

	_title = Label.new()
	_title.clip_text = true
	_title.tooltip_text = ""
	add_child(_title)

	# A dark plate behind the picture, so a white sprite and a transparent one are both visible.
	var plate := PanelContainer.new()
	var style := StyleBoxFlat.new()
	style.bg_color = Color(0.13, 0.13, 0.15)
	style.set_corner_radius_all(4)
	style.set_content_margin_all(6)
	plate.add_theme_stylebox_override("panel", style)
	plate.custom_minimum_size.y = 240
	add_child(plate)

	_preview = TextureRect.new()
	_preview.expand_mode = TextureRect.EXPAND_IGNORE_SIZE
	_preview.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED
	_preview.texture_filter = CanvasItem.TEXTURE_FILTER_NEAREST
	plate.add_child(_preview)

	_preview_note = Label.new()
	_preview_note.horizontal_alignment = HORIZONTAL_ALIGNMENT_CENTER
	_preview_note.modulate = Color(1, 1, 1, 0.6)
	add_child(_preview_note)

	var scroll := ScrollContainer.new()
	scroll.size_flags_vertical = Control.SIZE_EXPAND_FILL
	scroll.horizontal_scroll_mode = ScrollContainer.SCROLL_MODE_DISABLED
	add_child(scroll)

	var column := VBoxContainer.new()
	column.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	scroll.add_child(column)

	_facts = VBoxContainer.new()
	_facts.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	column.add_child(_facts)

	_tags = RichTextLabel.new()
	_tags.bbcode_enabled = true
	_tags.fit_content = true
	_tags.custom_minimum_size.y = 0
	column.add_child(_tags)

	_variants_box = VBoxContainer.new()
	column.add_child(_variants_box)
	var variants_label := Label.new()
	variants_label.text = "Formats"
	variants_label.modulate = Color(1, 1, 1, 0.6)
	_variants_box.add_child(variants_label)
	_variants_row = HBoxContainer.new()
	_variants_box.add_child(_variants_row)

	var actions := HBoxContainer.new()
	add_child(actions)

	_import_button = Button.new()
	_import_button.text = "Import"
	_import_button.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_import_button.pressed.connect(func(): import_requested.emit(_current))
	actions.add_child(_import_button)

	_open_button = Button.new()
	_open_button.text = "Open in Ambar"
	_open_button.tooltip_text = "Opens this asset's page in your browser"
	_open_button.pressed.connect(_open_in_browser)
	actions.add_child(_open_button)

	_show_empty(true)


func set_imported(imported: Dictionary) -> void:
	_imported = imported
	if not _current.is_empty():
		_update_import_button()


## show_asset fills the panel for one asset from the grid, then fetches the rest.
##
## The tile's own row is enough for the title and the picture, so those appear immediately and the
## request only adds to them; a panel that blanks for half a second on every arrow key is worse
## than one that fills in.
func show_asset(a: Dictionary) -> void:
	_request += 1
	var request := _request
	_current = a
	_show_empty(false)

	var id := int(a.get("id", 0))
	_title.text = String(a.get("filename", ""))
	_title.tooltip_text = String(a.get("path", ""))
	_preview.texture = null
	_preview_note.text = "Loading…"
	_load_preview(id, request)
	_render_facts(a, {})
	_tags.text = ""
	_variants_box.visible = false
	_update_import_button()

	var api: RefCounted = _api_factory.call()
	api.asset(id, func(ok, result):
		if request != _request:
			return # a newer selection won
		if not ok or not result is Dictionary:
			_tags.text = "[color=#ff8080]%s[/color]" % str(result)
			return
		var full: Dictionary = result.get("asset", {}) if result.get("asset") is Dictionary else a
		_render_facts(full, result)
		_render_tags(result.get("tags", []))
		_render_variants(result.get("variants", []), int(full.get("id", id)))
	)


func _show_empty(empty: bool) -> void:
	_empty.visible = empty
	for control in [_title, _preview_note, _facts, _tags, _import_button, _open_button]:
		control.visible = not empty
	_preview.get_parent().visible = not empty
	_variants_box.visible = false


# --- the picture ---------------------------------------------------------------------

## _load_preview asks for the full-size preview and falls back to the big thumbnail.
##
## Both can legitimately be missing — a model has no image preview until Blender has run, and an
## unsupported format (the specification records `.tga` and `.xcf` as such) has neither — so a failure says so
## rather than leaving an empty box that reads as a broken panel.
func _load_preview(asset_id: int, request: int) -> void:
	var api: RefCounted = _api_factory.call()
	api.fetch_bytes(api.preview_url(asset_id), func(ok, bytes):
		if request != _request:
			return
		if ok:
			var texture := _decode(bytes)
			if texture != null:
				_show_texture(texture)
				return
		var fallback: RefCounted = _api_factory.call()
		fallback.fetch_bytes(fallback.thumb2x_url(asset_id), func(ok2, bytes2):
			if request != _request:
				return
			var texture2 := _decode(bytes2) if ok2 else null
			if texture2 != null:
				_show_texture(texture2)
				return
			_render_model(asset_id, request)
		)
	)


## _render_model draws a model the server has no picture of, here, and posts it back.
##
## The panel is where somebody has *asked* about one asset, so it is worth the render even when
## the grid has moved on — and a model with no thumbnail is the common case, not an error: the
## server has no renderer and Blender is optional. See model_render.gd.
func _render_model(asset_id: int, request: int) -> void:
	var links: Dictionary = _current.get("links", {}) if _current.get("links") is Dictionary else {}
	if _renderer == null or not links.has("preview_glb"):
		_preview_note.text = _no_preview_reason()
		return

	_preview_note.text = "Rendering…"
	var api: RefCounted = _api_factory.call()
	api.fetch_bytes(api.preview_glb_url(asset_id), func(ok, glb):
		if request != _request:
			return
		if not ok:
			_preview_note.text = _no_preview_reason()
			return
		_draw_model(asset_id, request, glb)
	)


func _draw_model(asset_id: int, request: int, glb: PackedByteArray) -> void:
	var image: Image = await _renderer.render(glb)
	if request != _request:
		return
	if image == null:
		_preview_note.text = "That model would not draw."
		return
	_show_texture(ImageTexture.create_from_image(image))
	_preview_note.text += "  ·  rendered here"
	var api: RefCounted = _api_factory.call()
	api.upload_thumb(asset_id, image.save_png_to_buffer(), func(ok, message):
		if not ok:
			push_warning("Ambar: rendered asset %d but could not store it — %s" % [asset_id, message])
	)


## _no_preview_reason says which of the two reasons applies, because they need different things
## from the person reading it.
func _no_preview_reason() -> String:
	if String(_current.get("kind", "")) == "model":
		# derive records this as needs_blender: no pure-Go reader, and Godot has no runtime FBX
		# importer either, so neither end of this can draw it.
		return "No preview: this format needs Blender on the server."
	return "No preview for this file yet."


## _show_texture puts the picture on the plate, upscaling small images by a whole number first.
##
## Nearest filtering alone is not enough for pixel art: a 32-pixel sprite stretched to 340 by
## 10.6× gives some source pixels eleven screen pixels and others ten, so a uniform grid comes out
## visibly uneven. Resizing the image by an integer factor and letting the TextureRect centre it
## keeps every pixel the same size. the specification calls bilinear-scaled pixel art the most annoying failure of
## every existing tool; unevenly nearest-scaled is the next one along.
func _show_texture(texture: ImageTexture) -> void:
	var source := texture.get_size()
	var note := "preview %d×%d" % [int(source.x), int(source.y)]

	var plate: Control = _preview.get_parent()
	var room := plate.size - Vector2(16, 16)
	if source.x > 0 and source.y > 0 and room.x > 0 and room.y > 0:
		var factor := int(floor(minf(room.x / source.x, room.y / source.y)))
		if factor >= 2:
			var image := texture.get_image()
			image.resize(int(source.x) * factor, int(source.y) * factor, Image.INTERPOLATE_NEAREST)
			texture = ImageTexture.create_from_image(image)
			note += "  ·  %d×" % factor
			# An exact multiple must not then be re-scaled by a fraction to "fit".
			_preview.stretch_mode = TextureRect.STRETCH_KEEP_CENTERED
		else:
			_preview.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED

	_preview.texture = texture
	_preview_note.text = note


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


# --- the facts -----------------------------------------------------------------------

func _render_facts(a: Dictionary, detail: Dictionary) -> void:
	for child in _facts.get_children():
		child.queue_free()

	_fact("Kind", String(a.get("kind", "")))

	var w := int(a.get("width", 0))
	var h := int(a.get("height", 0))
	if w > 0 and h > 0:
		_fact("Pixels", "%d × %d" % [w, h])
	if int(a.get("frame_count", 0)) > 1:
		# The grid is only known for a detected spritesheet; an animated .aseprite has frames and
		# no geometry, and "(0×0)" is worse than saying nothing.
		var cols := int(a.get("frame_cols", 0))
		var rows := int(a.get("frame_rows", 0))
		if cols > 0 and rows > 0:
			_fact("Frames", "%d  (%d×%d grid)" % [int(a.get("frame_count")), cols, rows])
		else:
			_fact("Frames", str(int(a.get("frame_count"))))
	if int(a.get("duration_ms", 0)) > 0:
		_fact("Length", "%.1f s · %d Hz" % [int(a.get("duration_ms")) / 1000.0, int(a.get("sample_rate", 0))])
	if int(a.get("tri_count", 0)) > 0:
		_fact("Geometry", "%s triangles · %s vertices" % [
			_thousands(int(a.get("tri_count"))), _thousands(int(a.get("vert_count", 0)))])
	_fact("File", "%s · .%s" % [_bytes(int(a.get("size", 0))), String(a.get("ext", ""))])
	if bool(a.get("is_pixel_art", false)):
		_fact("Looks like", "pixel art")

	var pack: Dictionary = a.get("pack", {}) if a.get("pack") is Dictionary else {}
	_fact("Pack", String(pack.get("name", "—")))

	var prov: Dictionary = detail.get("provenance", {}) if detail.get("provenance") is Dictionary else {}
	if not prov.is_empty():
		var license := String(prov.get("license", ""))
		var author := String(prov.get("author", ""))
		# Licences are recorded per pack, and saying so here stops somebody reading it as a
		# statement about this one file.
		_fact("Licence", license if license != "" else "not recorded", "pack-level")
		if author != "":
			_fact("Author", author)

	# The library path is the one field long enough to fill the panel on its own, and it is
	# reference material rather than something to read every time — one line, ellipsis, tooltip.
	_fact("Path", String(a.get("path", "")), "", false)


func _fact(key: String, value: String, hint: String = "", wrap: bool = true) -> void:
	var row := HBoxContainer.new()
	var key_label := Label.new()
	key_label.text = key
	key_label.custom_minimum_size.x = 84
	key_label.modulate = Color(1, 1, 1, 0.6)
	key_label.vertical_alignment = VERTICAL_ALIGNMENT_TOP
	row.add_child(key_label)

	var value_label := Label.new()
	value_label.text = value
	if wrap:
		value_label.autowrap_mode = TextServer.AUTOWRAP_WORD_SMART
	else:
		value_label.text_overrun_behavior = TextServer.OVERRUN_TRIM_ELLIPSIS
	value_label.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	value_label.tooltip_text = value if hint == "" else "%s — %s" % [value, hint]
	row.add_child(value_label)

	_facts.add_child(row)


func _render_tags(tags: Array) -> void:
	if tags.is_empty():
		_tags.text = "[color=#888]no tags[/color]"
		return
	var parts: Array = []
	for tag in tags:
		parts.append("[bgcolor=#3a3a44] %s [/bgcolor]" % String(tag))
	_tags.text = " ".join(PackedStringArray(parts))


## _render_variants offers the other formats of the same artwork. Which one to import is a
## real choice — the PNG for the game, the ASEPRITE to keep editing — and it is the reason the grid
## can collapse them without hiding anything.
func _render_variants(variants: Array, current_id: int) -> void:
	for child in _variants_row.get_children():
		child.queue_free()
	if variants.size() < 2:
		_variants_box.visible = false
		return
	_variants_box.visible = true

	for raw in variants:
		if not raw is Dictionary:
			continue
		var v: Dictionary = raw
		var button := Button.new()
		button.text = "." + String(v.get("ext", "?"))
		button.toggle_mode = true
		button.set_pressed_no_signal(int(v.get("id", 0)) == current_id)
		# The path, not just the filename: a model shipped as Assets/fbx/thing.fbx and
		# Assets/fbx(unity)/thing.fbx gives two buttons reading ".fbx", and only the folder
		# tells them apart.
		button.tooltip_text = "%s\n%s" % [String(v.get("path", v.get("filename", ""))), _bytes(int(v.get("size", 0)))]
		button.set_meta("asset_id", int(v.get("id", 0)))
		button.pressed.connect(func(): _choose_variant(v))
		_variants_row.add_child(button)


func _choose_variant(v: Dictionary) -> void:
	_current = v
	_title.text = String(v.get("filename", ""))
	_title.tooltip_text = String(v.get("path", ""))
	_render_facts(v, {})
	_update_import_button()
	var chosen := int(v.get("id", 0))
	for child in _variants_row.get_children():
		if child is Button:
			child.set_pressed_no_signal(int(child.get_meta("asset_id", 0)) == chosen)
	# The preview follows the chosen format, because a .psd and its .png do not always look alike.
	_request += 1
	_preview.texture = null
	_preview_note.text = "Loading…"
	_load_preview(int(v.get("id", 0)), _request)


func _update_import_button() -> void:
	var id := str(int(_current.get("id", 0)))
	if _imported.has(id):
		_import_button.text = "In project"
		_import_button.disabled = true
		var entry: Dictionary = _imported[id] if _imported[id] is Dictionary else {}
		_import_button.tooltip_text = String(entry.get("res_path", ""))
	else:
		_import_button.text = "Import"
		_import_button.disabled = false
		_import_button.tooltip_text = "Download into res://assets/<kind>/<pack>/"


func _open_in_browser() -> void:
	var api: RefCounted = _api_factory.call()
	var id := int(_current.get("id", 0))
	if id > 0:
		OS.shell_open(api.web_url(id))


# --- formatting ----------------------------------------------------------------------

static func _bytes(n: int) -> String:
	if n < 1024:
		return "%d B" % n
	if n < 1024 * 1024:
		return "%.1f KB" % (n / 1024.0)
	if n < 1024 * 1024 * 1024:
		return "%.1f MB" % (n / 1048576.0)
	return "%.2f GB" % (n / 1073741824.0)


static func _thousands(n: int) -> String:
	var text := str(n)
	var out := ""
	var count := 0
	for i in range(text.length() - 1, -1, -1):
		out = text[i] + out
		count += 1
		if count % 3 == 0 and i > 0:
			out = "," + out
	return out
