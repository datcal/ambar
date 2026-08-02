@tool
extends EditorPlugin
## Ambar editor plugin.
##
## This was rebuilt once already. The first version added a dock in DOCK_SLOT_LEFT_UR and kept its
## connection settings in Editor Settings; installing it appeared to do nothing at all. Three
## reasons, all of them fair:
##
##   1. It reached for the `EditorInterface` singleton, which exists only in Godot 4.2+. On 4.1
##      the script fails to load, so the plugin does not appear at all. editor_compat.gd now
##      handles both.
##   2. A dock tab in the upper-left slot is easy to miss entirely, and it is not what anyone
##      means by "a menu up there next to 2D and 3D". That is a *main screen* plugin, which is
##      what this is now: `_has_main_screen()` gives us a tab beside 2D, 3D and Script.
##   3. The server URL and token lived in Editor Settings under `ambar/base_url`, which nobody
##      finds, so the default `http://ambar:8973` stayed and every request went nowhere. They are
##      asked for in our own UI now, with a button that tests the connection and says what came
##      back.

const Main := preload("res://addons/ambar/main.gd")
const Compat := preload("res://addons/ambar/editor_compat.gd")

# The tab mark, drawn on a 16-unit grid because 16 px is the size it is used at. See _mark().
const MARK_SVG := """<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" width="16" height="16">
<g fill="#c8cedb"><rect x="2" y="2" width="5" height="5" rx="1.2"/><rect x="2" y="9" width="5" height="5" rx="1.2"/><rect x="9" y="9" width="5" height="5" rx="1.2"/></g>
<rect x="8" y="2" width="6" height="6" rx="1.4" fill="#f0b429"/>
</svg>"""

var _main: Control
# Whether the dock fallback was used. remove_control_from_docks() errors on a control it never
# received, and Godot 4.7 reports that as a condition failure on shutdown.
var _in_dock := false


func _enter_tree() -> void:
	_main = Main.new()
	_main.name = "Ambar"
	_main.hide()

	# The main screen is where the 2D/3D/Script tabs put their contents. Adding our control there
	# and answering _has_main_screen() is what produces the tab itself.
	#
	# Parented *before* set_plugin, and that order matters: an HTTPRequest refuses to run unless it
	# is inside the tree, so the first search — which fires as soon as the plugin knows it is
	# configured — failed with "could not send the request" when this was the other way round.
	# Found by running it, not by reading it.
	var screen := Compat.main_screen(self)
	if screen != null:
		screen.add_child(_main)
		_main.set_anchors_and_offsets_preset(Control.PRESET_FULL_RECT)
	else:
		# No main screen available (a Godot version this does not know): fall back to a dock, so
		# the plugin is reachable rather than invisible.
		add_control_to_dock(DOCK_SLOT_LEFT_UR, _main)
		_in_dock = true
		_main.show()
		push_warning("Ambar: this Godot version exposes no editor main screen; using a dock instead")

	_main.set_plugin(self)


func _exit_tree() -> void:
	if _main == null:
		return
	if _in_dock:
		remove_control_from_docks(_main)
		_in_dock = false
	elif _main.get_parent() != null:
		_main.get_parent().remove_child(_main)
	_main.queue_free()
	_main = null


# --- main screen contract ------------------------------------------------------------
#
# These four are what Godot asks of a plugin that wants its own tab.

func _has_main_screen() -> bool:
	return true


func _get_plugin_name() -> String:
	return "Ambar"


func _get_plugin_icon() -> Texture2D:
	var icon := _mark()
	if icon != null:
		return icon
	# The editor's own icons, if we could not draw ours. "FileThumbnail" reads as "a library of
	# files" and follows the theme, which is what the tab used before it had a mark of its own.
	var themed := Compat.theme_icon(self, "FileThumbnail")
	if themed != null:
		return themed
	return Compat.theme_icon(self, "Filesystem")


## _mark rasterises the Ambar mark at the editor's scale.
##
## Drawn from a string rather than shipped as icon.svg, because a resource file has to go through
## Godot's importer before `preload` can resolve it — and a plugin whose script fails to compile is
## enabled, inert, and silent apart from one line in the Output panel. This addon has shipped that
## failure once already; a string cannot fail to import.
##
## The mark is the library grid with one tile selected. The selected tile is *larger* as well as
## amber, because Godot tints main-screen button icons: where the tint wins, the colour is gone and
## the size is all that is left to say which tile is the one you picked.
##
## Not the same drawing as docs/icon.png, which is the Asset Library listing icon. That one has a
## 3x3 grid on a dark plate, which is right at 128 px and a smudge at 16.
func _mark() -> Texture2D:
	var img := Image.new()
	# Godot builds without the SVG module cannot do this. Editor builds always can, but asking
	# costs nothing and the fallback above is already there.
	if not img.has_method("load_svg_from_string"):
		return null
	if img.call("load_svg_from_string", MARK_SVG, Compat.editor_scale(self)) != OK:
		return null
	return ImageTexture.create_from_image(img)


func _make_visible(visible: bool) -> void:
	if _main == null:
		return
	_main.visible = visible
	# Switching to the tab is the last chance to notice that the first page was never fetched —
	# after a connection was configured in another session, say. It loads once and then stops
	# asking, so flipping between 2D and Ambar does not re-query the library every time.
	if visible and _main.has_method("on_shown"):
		_main.on_shown()
