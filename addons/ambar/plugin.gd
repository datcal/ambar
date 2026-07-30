@tool
extends EditorPlugin
## Ambar editor plugin (§10). Adds a dock that searches the library and imports
## assets into res://assets/… with the correct Godot import presets, recording
## each import back to the server for credits and "already imported" badges.

const Dock := preload("res://addons/ambar/dock.gd")

# Editor Settings keys (§10: "one configured base URL plus an API token").
const SETTING_BASE_URL := "ambar/base_url"
const SETTING_TOKEN := "ambar/api_token"

var _dock: Control


func _enter_tree() -> void:
	_register_settings()
	_dock = Dock.new()
	_dock.name = "Ambar"
	_dock.set_plugin(self)
	add_control_to_dock(DOCK_SLOT_LEFT_UR, _dock)


func _exit_tree() -> void:
	if _dock:
		remove_control_from_docks(_dock)
		_dock.queue_free()
		_dock = null


func _register_settings() -> void:
	var es := EditorInterface.get_editor_settings()
	if not es.has_setting(SETTING_BASE_URL):
		es.set_setting(SETTING_BASE_URL, "http://ambar:8973")
	if not es.has_setting(SETTING_TOKEN):
		es.set_setting(SETTING_TOKEN, "")
	# Mark the token as a password-style field so it is not shown in plain sight.
	es.add_property_info({
		"name": SETTING_TOKEN,
		"type": TYPE_STRING,
		"hint": PROPERTY_HINT_PASSWORD,
	})


func base_url() -> String:
	return String(EditorInterface.get_editor_settings().get_setting(SETTING_BASE_URL)).rstrip("/")


func api_token() -> String:
	return String(EditorInterface.get_editor_settings().get_setting(SETTING_TOKEN))
