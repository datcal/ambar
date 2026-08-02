@tool
extends RefCounted
## Editor APIs that moved between Godot versions, in one place.
##
## This file exists because of the most likely reason the previous plugin "did nothing": it
## called the `EditorInterface` *singleton*, which only exists in Godot 4.2 and later. On 4.0 or
## 4.1 that is a parse-time identifier error, the plugin fails to load, and the editor reports it
## in a place nobody looks — so from the outside, installing the addon has no visible effect at
## all.
##
## Every call here degrades to something harmless rather than erroring, because a plugin that
## half-works and says so beats a plugin that disappears.


## editor_interface returns the EditorInterface, whichever way this Godot exposes it.
static func editor_interface(plugin: EditorPlugin):
	# 4.2+: a global singleton. Checked with a string so 4.1 never has to resolve the name.
	#
	# `has_singleton` answers true outside the editor too — the name is registered, the object is
	# not — and fetching it there prints "Can't retrieve singleton 'EditorInterface' outside of
	# editor" into the Output panel. Harmless, and exactly the kind of red text that gets blamed
	# for whatever else went wrong that day.
	if Engine.is_editor_hint() and Engine.has_singleton("EditorInterface"):
		return Engine.get_singleton("EditorInterface")
	# 4.0/4.1: a method on the plugin. `has_method` keeps this from being an error either way.
	if plugin != null and plugin.has_method("get_editor_interface"):
		return plugin.call("get_editor_interface")
	return null


## main_screen is the container the 2D/3D/Script tabs live in. Null when unavailable, which the
## caller must treat as "no main screen tab for us".
static func main_screen(plugin: EditorPlugin) -> Node:
	var ei = editor_interface(plugin)
	if ei != null and ei.has_method("get_editor_main_screen"):
		return ei.call("get_editor_main_screen")
	return null


## rescan asks the editor to notice files that appeared on disk. Without it an imported asset is
## in the project folder but not in the FileSystem dock until the editor is refocused, which
## looks exactly like a failed import.
static func rescan(plugin: EditorPlugin) -> void:
	var ei = editor_interface(plugin)
	if ei == null or not ei.has_method("get_resource_filesystem"):
		return
	var fs = ei.call("get_resource_filesystem")
	if fs != null and fs.has_method("scan"):
		fs.call("scan")


## reveal selects a file in the editor's FileSystem dock, so "where did this land" is one click
## rather than a path to go and find by hand. Silent where the editor does not offer it.
static func reveal(plugin: EditorPlugin, res_path: String) -> void:
	var ei = editor_interface(plugin)
	if ei == null or not ei.has_method("select_file"):
		return
	ei.call("select_file", res_path)


## theme_icon fetches an editor icon by name, or null. Used so the tab and the buttons look like
## the rest of the editor instead of shipping images.
static func theme_icon(plugin: EditorPlugin, name: String) -> Texture2D:
	var ei = editor_interface(plugin)
	if ei == null or not ei.has_method("get_base_control"):
		return null
	var base = ei.call("get_base_control")
	if base == null:
		return null
	var theme: Theme = base.get_theme()
	if theme == null or not theme.has_icon(name, "EditorIcons"):
		return null
	return theme.get_icon(name, "EditorIcons")
