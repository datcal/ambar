@tool
extends Control
## The Ambar main screen: connect, browse, inspect, import.
##
## An earlier version was a search box and an ItemList in a dock tab nobody had open. This is a main
## screen that opens on a setup panel until the connection works and shows thumbnails rather than a
## list of "1.png, 1.png, 1.png".
##
## What that grid could do was then rebuilt from the report on using it: five tiles across a
## window that fits fifteen, one fixed thumbnail size, no order but filename, no way to look at
## anything without importing it first, and "Load more" instead of pages. Each of those is now a
## control in the toolbar, a panel on the right, or a pager under the grid — and each needed the
## API to grow a parameter, which is why the specification changed in the same milestone.

const Api := preload("res://addons/ambar/api.gd")
const Config := preload("res://addons/ambar/config.gd")
const Project := preload("res://addons/ambar/project.gd")
const Compat := preload("res://addons/ambar/editor_compat.gd")
const Importer := preload("res://addons/ambar/importer.gd")
const Browser := preload("res://addons/ambar/browser.gd")
const Detail := preload("res://addons/ambar/detail.gd")
const ModelRender := preload("res://addons/ambar/model_render.gd")
const ProjectView := preload("res://addons/ambar/project_view.gd")

## The tile sizes the toolbar offers. 64 fits a lot of pixel art on screen at once; 256 is for
## deciding whether a character sheet is the one you remember.
const TILE_SIZES := [64, 96, 128, 192, 256]
## Page sizes. 500 is the server's MaxPageSize and there is no point offering more than it honours.
const PAGE_SIZES := [30, 60, 120, 240]
## How wide the inspector opens, and the narrowest it can be dragged to before its own contents
## start clipping.
const DEFAULT_PANEL_WIDTH := 340
const MIN_PANEL_WIDTH := 220

var _plugin: EditorPlugin

# Setup panel
var _setup: VBoxContainer
var _url_field: LineEdit
var _token_field: LineEdit
var _setup_status: RichTextLabel

# Browse panel
var _browse: VBoxContainer
var _tabs: TabBar
var _library: VBoxContainer
var _project: ProjectView
var _search: LineEdit
var _kind: OptionButton
var _sort: OptionButton
var _tile_size_picker: OptionButton
var _page_size_picker: OptionButton
var _split: HSplitContainer
var _grid: Browser
var _detail: Detail
var _renderer: Node
var _panel_renderer: Node

var _page := 1
var _imported: Dictionary = {} # asset id (string) → manifest entry
var _searching := false
## The order the visible results were actually fetched in, so the dropdown can never end up
## describing an order the grid is not in.
var _sort_applied := ""
## Whether the first page has been fetched. The panel is built, parented and shown in an order
## that varies between the editor, the dock fallback and the test harness, so "have I loaded" is
## the only condition that means the same thing in all three.
var _loaded_once := false


func set_plugin(p: EditorPlugin) -> void:
	_plugin = p
	_build()
	_imported = Project.manifest()
	if Config.configured():
		_show_browse()
		_load_initial()
	else:
		_show_setup()


func _ready() -> void:
	_load_initial()


## on_shown is called when the editor switches to the Ambar tab.
func on_shown() -> void:
	_load_initial()


## _load_initial fetches the first page, once, as soon as it is possible to.
##
## This is three entry points on purpose, because the one it used to have was the wrong one and the
## tab opened empty every single time. `_ready()` looked like the safe moment — the control is in
## the tree, so an HTTPRequest will run — but plugin.gd parents the control *before* it calls
## `set_plugin`, precisely so that is true, which means `_ready` fires while `_plugin` is still
## null and the guard that was watching for it never let the search start. The panel then only
## filled in when something else called `_do_search`: "Save and test" on the server panel, which is
## why re-testing a connection that was already working appeared to be what loaded the library.
##
## So: whichever of the three happens first wins, and the guard is "have I loaded", not "does some
## other object exist yet".
func _load_initial() -> void:
	if _loaded_once or not Config.configured():
		return
	if _grid == null:
		# `_ready` also fires before `set_plugin`, so the panel may not be built yet. The call
		# from `set_plugin` — which runs immediately after `_build` — is the one that gets through.
		return
	if not is_inside_tree():
		# Nothing can be fetched from outside the tree. Ask again on the way in.
		if not tree_entered.is_connected(_load_initial):
			tree_entered.connect(_load_initial)
		return
	_loaded_once = true
	_load_sorts()
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

	# Two screens, because they answer opposite questions: "what is in the library" and "what did
	# I take from it". The second one had no home at all — the manifest has been committed to
	# every project since M9 and its only visible trace was a tile going grey somewhere in a
	# search — so the credits action lives there now too, beside the thing it describes.
	_tabs = TabBar.new()
	_tabs.add_tab("Library")
	_tabs.add_tab("In this project")
	_tabs.tab_changed.connect(_on_tab_changed)
	_browse.add_child(_tabs)

	_library = VBoxContainer.new()
	_library.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_browse.add_child(_library)

	_library.add_child(_build_toolbar())

	# The grid and the inspector, with a handle between them: how much room the panel deserves
	# depends on whether you are hunting or deciding, and that changes minute to minute.
	_split = HSplitContainer.new()
	_split.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_split.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_library.add_child(_split)

	# Two renderers, not one. Each owns a SubViewport and draws one model at a time, and a page of
	# thirty models is a minute of work — behind which the panel would sit saying "Rendering…"
	# every time somebody clicked a tile. The one the person is looking at gets its own.
	#
	# Parented to the browse panel so they are in the tree: a viewport outside it renders nothing.
	_renderer = ModelRender.new()
	_renderer.name = "AmbarGridRender"
	_library.add_child(_renderer)

	_panel_renderer = ModelRender.new()
	_panel_renderer.name = "AmbarPanelRender"
	_library.add_child(_panel_renderer)

	_grid = Browser.new()
	_split.add_child(_grid)
	_grid.setup(_api, _tile_size(), _renderer)
	_grid.selected.connect(_on_selected)
	_grid.activated.connect(_on_activated)
	_grid.page_requested.connect(_on_page_requested)
	_grid.set_imported(_imported)

	_detail = Detail.new()
	_split.add_child(_detail)
	_detail.setup(_api, _panel_renderer)
	_detail.import_requested.connect(_on_import_requested)
	_detail.set_imported(_imported)

	# `split_offset` is measured from the *end of the first child*, not from the middle, so a
	# remembered offset is meaningless once the window is a different width — and a negative one
	# clamps to zero, which is a one-tile-wide grid beside an inspector the size of the screen.
	# What is worth remembering is the panel's width; the offset is derived from it every resize.
	_split.resized.connect(_apply_panel_width)
	_split.dragged.connect(func(offset: int):
		Config.set_pref("panel_width", max(MIN_PANEL_WIDTH, int(_split.size.x) - offset))
	)

	_project = ProjectView.new()
	# Flags before parenting, hidden after: a container sorts what it is given when it is given
	# it, and a control that arrives hidden and flagless is the kind of thing that comes back at
	# its minimum size later. Cheap insurance, not a fix for anything observed.
	_project.size_flags_vertical = Control.SIZE_EXPAND_FILL
	_project.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_browse.add_child(_project)
	_project.visible = false
	_project.setup(_api, _plugin)
	# An update or a removal there changes what the library grid should be badging.
	_project.changed.connect(func():
		_imported = Project.manifest()
		_grid.set_imported(_imported)
		_detail.set_imported(_imported)
		_update_project_tab()
	)
	_update_project_tab()


func _build_toolbar() -> HBoxContainer:
	var bar := HBoxContainer.new()

	_search = LineEdit.new()
	_search.placeholder_text = "Search the library — sword, type:model, 32x32, theme:sci-fi"
	_search.size_flags_horizontal = Control.SIZE_EXPAND_FILL
	_search.clear_button_enabled = true
	_search.text_submitted.connect(func(_t): _do_search(true))
	bar.add_child(_search)

	var go := Button.new()
	go.text = "Search"
	go.pressed.connect(func(): _do_search(true))
	bar.add_child(go)

	_kind = OptionButton.new()
	_kind.tooltip_text = "Filter by kind"
	for k in ["any", "image", "model", "audio", "font", "tilemap"]:
		_kind.add_item(k)
	_kind.select(_index_of_text(_kind, String(Config.pref("kind", "any"))))
	_kind.item_selected.connect(func(_i):
		Config.set_pref("kind", _kind.get_item_text(_kind.selected))
		_do_search(true)
	)
	bar.add_child(_kind)

	# Populated from /api/v1/sorts once connected; one item until then so the control is never an
	# empty dropdown somebody clicks into. That item carries the *saved* order rather than the
	# default, because the first search fires before the list arrives — with "added" hardcoded
	# here, somebody who browses by name got the dropdown saying "Name A→Z" over a grid that was
	# still in arrival order.
	_sort = OptionButton.new()
	_sort.tooltip_text = "Browse order"
	var saved_sort := String(Config.pref("sort", "added"))
	_sort.add_item(saved_sort)
	_sort.set_item_metadata(0, saved_sort)
	_sort.item_selected.connect(func(_i):
		Config.set_pref("sort", String(_sort.get_selected_metadata()))
		_do_search(true)
	)
	bar.add_child(_sort)

	_tile_size_picker = OptionButton.new()
	_tile_size_picker.tooltip_text = "Thumbnail size"
	for i in TILE_SIZES.size():
		_tile_size_picker.add_item("%d px" % TILE_SIZES[i])
		_tile_size_picker.set_item_metadata(i, TILE_SIZES[i])
		if TILE_SIZES[i] == _tile_size():
			_tile_size_picker.select(i)
	_tile_size_picker.item_selected.connect(func(i: int):
		var px := int(TILE_SIZES[i])
		Config.set_pref("tile_size", px)
		_grid.set_tile_size(px)
	)
	bar.add_child(_tile_size_picker)

	_page_size_picker = OptionButton.new()
	_page_size_picker.tooltip_text = "Assets per page"
	for i in PAGE_SIZES.size():
		_page_size_picker.add_item("%d / page" % PAGE_SIZES[i])
		_page_size_picker.set_item_metadata(i, PAGE_SIZES[i])
		if PAGE_SIZES[i] == _page_size():
			_page_size_picker.select(i)
	_page_size_picker.item_selected.connect(func(i: int):
		Config.set_pref("page_size", int(PAGE_SIZES[i]))
		_do_search(true)
	)
	bar.add_child(_page_size_picker)

	var settings := Button.new()
	settings.text = "Server…"
	settings.pressed.connect(_show_setup)
	bar.add_child(settings)

	return bar


## _on_tab_changed swaps the two screens. The project one reloads on every visit rather than
## caching: it is a comparison against files on disk and rows on a server, both of which move
## while the editor is open.
func _on_tab_changed(tab: int) -> void:
	var project_tab := tab == 1
	_library.visible = not project_tab
	_project.visible = project_tab
	# Same insurance: make the container lay out the screen that just appeared.
	_browse.queue_sort()
	if project_tab:
		_project.reload()


## _update_project_tab keeps the count in the tab label, so "did that import land" is answerable
## without switching to the other screen.
func _update_project_tab() -> void:
	if _tabs == null:
		return
	var count := _imported.size()
	_tabs.set_tab_title(1, "In this project" if count == 0 else "In this project (%d)" % count)


## _apply_panel_width keeps the inspector a fixed width as the editor window changes size, which
## is what an inspector should do: the grid takes the space, the panel keeps its shape.
func _apply_panel_width() -> void:
	if _split == null or _split.size.x <= 0:
		return
	var width := maxi(MIN_PANEL_WIDTH, int(Config.pref("panel_width", DEFAULT_PANEL_WIDTH)))
	var offset := maxi(0, int(_split.size.x) - width)
	if _split.split_offset != offset:
		_split.split_offset = offset


## _index_of_text finds a dropdown item by its label, for restoring a saved choice.
static func _index_of_text(option: OptionButton, text: String) -> int:
	for i in option.get_item_count():
		if option.get_item_text(i) == text:
			return i
	return 0


func _tile_size() -> int:
	return int(Config.pref("tile_size", 128))


func _page_size() -> int:
	return int(Config.pref("page_size", 60))


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
		_load_sorts()
		_do_search(true)
	)


func _api() -> RefCounted:
	return Api.new(Config.base_url(), Config.token(), self)


# --- browsing ----------------------------------------------------------------------

## _load_sorts fills the order dropdown from the server, so a new order added there appears here
## without a plugin release.
func _load_sorts() -> void:
	_api().sorts(func(ok, result):
		if not ok or not result is Dictionary:
			return
		var orders: Array = result.get("sorts", [])
		if orders.is_empty():
			return
		var wanted := String(Config.pref("sort", String(result.get("default", "added"))))
		_sort.clear()
		for i in orders.size():
			var order: Dictionary = orders[i] if orders[i] is Dictionary else {}
			_sort.add_item(String(order.get("label", order.get("value", "?"))))
			_sort.set_item_metadata(i, String(order.get("value", "")))
			if String(order.get("value", "")) == wanted:
				_sort.select(i)
		# If the list resolved to an order the visible results were not fetched in — a saved
		# preference the server no longer offers, say — fetch them again rather than leaving the
		# dropdown describing something the grid is not.
		if _selected_sort() != _sort_applied:
			_do_search(true)
	)


func _selected_sort() -> String:
	if _sort.selected < 0:
		return ""
	var value = _sort.get_selected_metadata()
	return String(value) if value != null else ""


func _do_search(reset_page: bool) -> void:
	if not Config.configured():
		_show_setup()
		return
	if reset_page:
		_page = 1
	_searching = true
	_grid.status("Searching…")

	var kind := _kind.get_item_text(_kind.selected) if _kind.selected >= 0 else "any"
	var page := _page
	_sort_applied = _selected_sort()
	_api().search(_search.text, kind, _sort_applied, page, _page_size(), func(ok, result):
		_searching = false
		if not ok:
			_grid.status("Search failed: %s" % str(result))
			return
		if not result is Dictionary:
			_grid.status("Unexpected answer from the server.")
			return
		# A page number beyond the end comes back empty rather than as an error; stepping back to
		# the last real page is friendlier than showing nothing after a filter change.
		var pages := int(result.get("pages", 1))
		if page > pages and pages >= 1:
			_page = pages
			_do_search(false)
			return
		_grid.show_page(result)
	)


func _on_page_requested(page: int) -> void:
	if _searching or page == _page:
		return
	_page = max(1, page)
	_do_search(false)


func _on_selected(a: Dictionary) -> void:
	_detail.show_asset(a)


func _on_activated(a: Dictionary) -> void:
	# Double-click is "I want this one", which is the shortcut worth having in a library you are
	# scanning quickly. It still goes through the panel so the outcome is reported in one place.
	_detail.show_asset(a)
	_on_import_requested(a)


func _on_import_requested(a: Dictionary) -> void:
	if a.is_empty():
		return
	_import(a)


# --- importing ---------------------------------------------------------------------

func _import(a: Dictionary) -> void:
	_grid.status("Importing %s…" % String(a.get("filename", "")))

	Importer.import_asset(_api(), a, func(ok, message, res_path):
		if not ok:
			_grid.status("Import failed: %s" % message)
			return
		Compat.rescan(_plugin)
		_imported = Project.manifest()
		_grid.set_imported(_imported)
		_detail.set_imported(_imported)
		_update_project_tab()
		_grid.status(message)
	)
