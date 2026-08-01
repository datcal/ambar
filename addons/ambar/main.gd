@tool
extends Control
## The Ambar main screen (§10): connect, browse, import.
##
## This replaces dock.gd, which was a search box and an ItemList in a dock tab. Two things are
## deliberately different:
##
##   * It opens on a setup panel until the connection works, and the panel *tests* the connection
##     and says what came back. The old plugin's failure mode was silence — an unreachable default
##     URL, an empty token, and an error label in a dock nobody had open.
##   * Results are thumbnails, not filenames. This is a library of pictures; a list of
##     "1.png, 1.png, 1.png" is not browsable, which the web grid had to learn too.
##
## Untested in a running editor: there is no Godot on the machine this was written on, and the Go
## suite cannot exercise GDScript. Everything here uses documented 4.x API and degrades rather
## than erroring, but it wants a real editor before it is trusted — see docs/decisions.md.

const Api := preload("res://addons/ambar/api.gd")
const Config := preload("res://addons/ambar/config.gd")
const Project := preload("res://addons/ambar/project.gd")
const Compat := preload("res://addons/ambar/editor_compat.gd")
const Importer := preload("res://addons/ambar/importer.gd")

const THUMB_SIZE := Vector2(96, 96)

var _plugin: EditorPlugin

# Setup panel
var _setup: VBoxContainer
var _url_field: LineEdit
var _token_field: LineEdit
var _setup_status: RichTextLabel

# Browse panel
var _browse: VBoxContainer
var _search: LineEdit
var _kind: OptionButton
var _grid: GridContainer
var _status: Label
var _more: Button

var _assets: Array = []
var _next_cursor := ""
var _imported: Dictionary = {} # asset id (string) → manifest entry


func set_plugin(p: EditorPlugin) -> void:
	_plugin = p
	_build()
	_imported = Project.manifest()
	# Which panel to show is safe to decide now; the first *request* is not, because an
	# HTTPRequest only runs once it is inside the tree. _ready() is the earliest point where that
	# is guaranteed.
	if Config.configured():
		_show_browse()
	else:
		_show_setup()


func _ready() -> void:
	if _plugin != null and Config.configured() and _assets.is_empty():
		_do_search(true)


# --- layout -------------------------------------------------------------------------

func _build() -> void:
	size_flags_horizontal = Control.SIZE_EXPAND_FILL
	size_flags_vertical = Control.SIZE_EXPAND_FILL

	var root := VBoxContainer.new()
	root.set_anchors_and_offsets_preset(Control.PRESET_FULL_RECT)
	add_child(root)

	_build_setup(root)
	_build_browse(root)


func _build_setup(root: VBoxContainer) -> void:
	_setup = VBoxContainer.new()
	_setup.size_flags_vertical = Control.SIZE_EXPAND_FILL
	root.add_child(_setup)

	var title := Label.new()
	title.text = "Connect to Ambar"
	_setup.add_child(title)

	var url_row := HBoxContainer.new()
	var url_label := Label.new()
	url_label.text = "Server"
	url_label.custom_minimum_size.x = 70
	url_row.add_child(url_label)
	_url_field = LineEdit.new()
	_url_field.placeholder_text = "http://meshnas.local:8973"
	_url_field.text = Config.base_url()
	_url_field.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	url_row.add_child(_url_field)
	_setup.add_child(url_row)

	var token_row := HBoxContainer.new()
	var token_label := Label.new()
	token_label.text = "Token"
	token_label.custom_minimum_size.x = 70
	token_row.add_child(token_label)
	_token_field = LineEdit.new()
	_token_field.placeholder_text = "paste an API token"
	_token_field.secret = true
	_token_field.text = Config.token()
	_token_field.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	token_row.add_child(_token_field)
	_setup.add_child(token_row)

	var buttons := HBoxContainer.new()
	var test := Button.new()
	test.text = "Save and test"
	test.pressed.connect(_on_save_and_test)
	buttons.add_child(test)

	var tokens_page := Button.new()
	tokens_page.text = "Create a token…"
	tokens_page.tooltip_text = "Opens Ambar's Settings → API tokens page in your browser"
	tokens_page.pressed.connect(func():
		var url := _url_field.text.strip_edges().rstrip("/")
		if url != "":
			OS.shell_open(url + "/settings/tokens")
	)
	buttons.add_child(tokens_page)

	var pixel := Button.new()
	pixel.text = "Set pixel-art import defaults"
	pixel.tooltip_text = "Nearest filtering, no mipmaps, lossless — for the whole project"
	pixel.pressed.connect(func(): _setup_status.text = Importer.apply_pixel_art_defaults())
	buttons.add_child(pixel)

	_setup.add_child(buttons)

	_setup_status = RichTextLabel.new()
	_setup_status.bbcode_enabled = true
	_setup_status.fit_content = true
	_setup_status.custom_minimum_size.y = 90
	_setup_status.text = "The server URL is saved in [code]res://ambar.cfg[/code] and shared with the project.\nThe token is personal: it stays in [code]%s[/code] and is never committed." % Config.token_file_path()
	_setup.add_child(_setup_status)


func _build_browse(root: VBoxContainer) -> void:
	_browse = VBoxContainer.new()
	_browse.size_flags_vertical = Control.SIZE_EXPAND_FILL
	root.add_child(_browse)

	var bar := HBoxContainer.new()
	_search = LineEdit.new()
	_search.placeholder_text = "Search the library — sword, type:model, 32x32, theme:sci-fi"
	_search.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_search.text_submitted.connect(func(_t): _do_search(true))
	bar.add_child(_search)

	_kind = OptionButton.new()
	for k in ["any", "image", "model", "audio", "font", "tilemap"]:
		_kind.add_item(k)
	_kind.item_selected.connect(func(_i): _do_search(true))
	bar.add_child(_kind)

	var go := Button.new()
	go.text = "Search"
	go.pressed.connect(func(): _do_search(true))
	bar.add_child(go)

	var credits := Button.new()
	credits.text = "Credits"
	credits.tooltip_text = "Write res://CREDITS.md from what this project has imported (§9)"
	credits.pressed.connect(_write_credits)
	bar.add_child(credits)

	var settings := Button.new()
	settings.text = "Server…"
	settings.pressed.connect(_show_setup)
	bar.add_child(settings)
	_browse.add_child(bar)

	var scroll := ScrollContainer.new()
	scroll.size_flags_vertical = Control.SIZE_EXPAND_FILL
	scroll.horizontal_scroll_mode = ScrollContainer.SCROLL_MODE_DISABLED
	_browse.add_child(scroll)

	_grid = GridContainer.new()
	_grid.columns = 6
	_grid.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	scroll.add_child(_grid)

	_more = Button.new()
	_more.text = "Load more"
	_more.visible = false
	_more.pressed.connect(func(): _do_search(false))
	_browse.add_child(_more)

	_status = Label.new()
	_status.text = ""
	_browse.add_child(_status)


func _show_setup() -> void:
	_setup.visible = true
	_browse.visible = false


func _show_browse() -> void:
	_setup.visible = false
	_browse.visible = true


# --- connecting ---------------------------------------------------------------------

func _on_save_and_test() -> void:
	var url := _url_field.text.strip_edges()
	if url != "" and not url.begins_with("http://") and not url.begins_with("https://"):
		url = "http://" + url # the common mistake, and a harmless thing to fix for someone
		_url_field.text = url

	Config.set_base_url(url)
	Config.set_token(_token_field.text)

	if url == "":
		_setup_status.text = "[color=#ff8080]Enter the address Ambar is served on.[/color]"
		return
	if _token_field.text.strip_edges() == "":
		_setup_status.text = "[color=#ff8080]Enter an API token — Ambar has no anonymous API.[/color]\nCreate one with the button above."
		return

	_setup_status.text = "Testing %s…" % url
	_api().ping(func(ok, result):
		if not ok:
			_setup_status.text = "[color=#ff8080]%s[/color]" % str(result)
			return
		var assets := 0
		if result is Dictionary:
			assets = int(result.get("assets", result.get("asset_count", 0)))
		if assets > 0:
			_setup_status.text = "[color=#7ec894]Connected.[/color] %d assets in the library." % assets
		else:
			_setup_status.text = "[color=#7ec894]Connected.[/color]"
		_show_browse()
		_do_search(true)
	)


func _api() -> RefCounted:
	return Api.new(Config.base_url(), Config.token(), self)


# --- browsing ----------------------------------------------------------------------

func _do_search(reset: bool) -> void:
	if not Config.configured():
		_show_setup()
		return
	if reset:
		_assets.clear()
		_next_cursor = ""
		for child in _grid.get_children():
			child.queue_free()
	_status.text = "Searching…"

	var kind := _kind.get_item_text(_kind.selected) if _kind.selected >= 0 else "any"
	_api().search(_search.text, kind, _next_cursor, func(ok, result):
		if not ok:
			_status.text = "Search failed: %s" % str(result)
			return
		if not result is Dictionary:
			_status.text = "Unexpected answer from the server."
			return

		var rows: Array = result.get("assets", [])
		for row in rows:
			if row is Dictionary:
				_assets.append(row)
				_add_tile(row)

		_next_cursor = String(result.get("next_cursor", ""))
		_more.visible = _next_cursor != ""
		var total := int(result.get("total", _assets.size()))
		_status.text = "%d of %d" % [_assets.size(), total]
	)


func _add_tile(a: Dictionary) -> void:
	var id := int(a.get("id", 0))
	var filename := String(a.get("filename", "asset"))

	var tile := VBoxContainer.new()
	tile.custom_minimum_size = Vector2(THUMB_SIZE.x + 16, THUMB_SIZE.y + 54)

	var picture := TextureRect.new()
	picture.custom_minimum_size = THUMB_SIZE
	picture.expand_mode = TextureRect.EXPAND_IGNORE_SIZE
	picture.stretch_mode = TextureRect.STRETCH_KEEP_ASPECT_CENTERED
	# Pixel art is the common case in this library, and a smoothed 32px sprite in a 96px box is
	# unreadable — the same lesson the web viewer had to learn the hard way.
	picture.texture_filter = CanvasItem.TEXTURE_FILTER_NEAREST
	tile.add_child(picture)
	_load_thumb(id, picture)

	var name_label := Label.new()
	name_label.text = filename
	name_label.tooltip_text = "%s · %s" % [filename, String(a.get("kind", ""))]
	name_label.clip_text = true
	name_label.custom_minimum_size.x = THUMB_SIZE.x
	tile.add_child(name_label)

	var add := Button.new()
	if _imported.has(str(id)):
		add.text = "In project"
		add.disabled = true
		add.tooltip_text = String(_imported[str(id)].get("res_path", ""))
	else:
		add.text = "Import"
		add.pressed.connect(func(): _import(a, add))
	tile.add_child(add)

	_grid.add_child(tile)


func _load_thumb(asset_id: int, into: TextureRect) -> void:
	var api := _api()
	api.fetch_bytes(api.thumb_url(asset_id), func(ok, bytes):
		if not ok or not into is TextureRect or not is_instance_valid(into):
			return
		var image := Image.new()
		# Thumbnails are WebP (§6). load_webp_from_buffer exists in 4.x; the PNG fallback covers a
		# server that ever serves something else.
		var err := image.load_webp_from_buffer(bytes)
		if err != OK:
			err = image.load_png_from_buffer(bytes)
		if err != OK:
			return
		into.texture = ImageTexture.create_from_image(image)
	)


# --- credits ------------------------------------------------------------------------

## _write_credits pulls the attribution file the server assembles from this project's recorded
## uses (§9: "generate an attribution file from what a project actually uses"). The server builds
## it because the server knows the licences; the plugin only knows what it imported.
func _write_credits() -> void:
	_status.text = "Fetching credits…"
	var api := _api()
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


# --- importing ---------------------------------------------------------------------

func _import(a: Dictionary, button: Button) -> void:
	button.disabled = true
	button.text = "…"
	_status.text = "Importing %s…" % String(a.get("filename", ""))

	Importer.import_asset(_api(), a, func(ok, message, res_path):
		if not ok:
			button.disabled = false
			button.text = "Import"
			_status.text = "Import failed: %s" % message
			return

		Compat.rescan(_plugin)
		button.text = "In project"
		button.tooltip_text = res_path
		_imported = Project.manifest()
		_status.text = message
	)
