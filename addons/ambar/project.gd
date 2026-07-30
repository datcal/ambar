@tool
extends RefCounted
## Project identity and manifest (§10). The project is identified by a UUID that
## lives in res://.ambar/project.json and is committed to git — never by a
## filesystem path (invariant 10), because two people check the project out at
## different paths. The manifest maps asset_id → provenance and is merged
## additively so one client never overwrites the other's imports.

const DIR := "res://.ambar"
const PROJECT_FILE := "res://.ambar/project.json"
const MANIFEST_FILE := "res://.ambar/manifest.json"


static func _ensure_dir() -> void:
	if not DirAccess.dir_exists_absolute(ProjectSettings.globalize_path(DIR)):
		DirAccess.make_dir_recursive_absolute(ProjectSettings.globalize_path(DIR))


## uuid returns the project's UUID, generating and persisting one on first use.
static func uuid() -> String:
	var data := _read_json(PROJECT_FILE)
	if data is Dictionary and data.has("uuid") and String(data["uuid"]) != "":
		return String(data["uuid"])
	var id := _generate_uuid()
	_ensure_dir()
	_write_json(PROJECT_FILE, {"uuid": id, "name": _project_name()})
	return id


static func name_hint() -> String:
	var data := _read_json(PROJECT_FILE)
	if data is Dictionary and data.has("name"):
		return String(data["name"])
	return _project_name()


## record adds an entry to the manifest, merging additively (§10: "treat it as
## shared state and merge additively, never rewrite the whole file").
static func record(asset_id: int, entry: Dictionary) -> void:
	var manifest := _read_json(MANIFEST_FILE)
	if not manifest is Dictionary:
		manifest = {}
	manifest[str(asset_id)] = entry
	_ensure_dir()
	_write_json(MANIFEST_FILE, manifest)


## manifest returns the current asset_id → entry map (string keys).
static func manifest() -> Dictionary:
	var m := _read_json(MANIFEST_FILE)
	return m if m is Dictionary else {}


static func _project_name() -> String:
	return String(ProjectSettings.get_setting("application/config/name", "Project"))


static func _generate_uuid() -> String:
	# RFC-4122-ish v4 from Godot's CSPRNG; good enough as an opaque project key.
	var b := Crypto.new().generate_random_bytes(16)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var h := b.hex_encode()
	return "%s-%s-%s-%s-%s" % [h.substr(0, 8), h.substr(8, 4), h.substr(12, 4), h.substr(16, 4), h.substr(20, 12)]


static func _read_json(path: String):
	if not FileAccess.file_exists(path):
		return null
	var f := FileAccess.open(path, FileAccess.READ)
	if f == null:
		return null
	var parsed = JSON.parse_string(f.get_as_text())
	f.close()
	return parsed


static func _write_json(path: String, value) -> void:
	var f := FileAccess.open(path, FileAccess.WRITE)
	if f == null:
		push_error("Ambar: cannot write " + path)
		return
	f.store_string(JSON.stringify(value, "  "))
	f.close()
