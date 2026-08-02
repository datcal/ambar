@tool
extends RefCounted
## Where the plugin's connection settings live.
##
## Not in Editor Settings, which is where the previous version put them. Two problems with that:
## nobody finds `ambar/base_url` in a list of four hundred editor preferences, so the default
## `http://ambar:8973` stayed — a hostname that resolves on nobody's network — and every search
## then failed silently. And reaching Editor Settings needs the `EditorInterface` singleton,
## which is the other reason the plugin did not load at all on Godot 4.1.
##
## The split matters for a team of five or six:
##
##   res://ambar.cfg          the server URL. A fact about the studio, so it is committed and
##                            everyone gets it by checking the project out.
##   user://ambar_token.cfg   the API token. Personal, per machine, never committed, for the
##                            same reason no password belongs in a repository.
##
## Both are plain ConfigFile, so either can be fixed in a text editor when something is wrong.

const PROJECT_FILE := "res://ambar.cfg"
const TOKEN_FILE := "user://ambar_token.cfg"
## Browse preferences: tile size, sort order, page size. Personal like the token but not secret
## like it, and a third file rather than a section in the second one because "delete my token"
## should not also mean "forget how I like the grid".
const PREFS_FILE := "user://ambar_prefs.cfg"


static func base_url() -> String:
	var cfg := ConfigFile.new()
	if cfg.load(PROJECT_FILE) != OK:
		return ""
	return String(cfg.get_value("server", "base_url", "")).strip_edges().rstrip("/")


static func set_base_url(url: String) -> void:
	var cfg := ConfigFile.new()
	cfg.load(PROJECT_FILE) # keep anything else that is in there
	cfg.set_value("server", "base_url", url.strip_edges().rstrip("/"))
	var err := cfg.save(PROJECT_FILE)
	if err != OK:
		push_error("Ambar: could not write %s (error %d)" % [PROJECT_FILE, err])


static func token() -> String:
	var cfg := ConfigFile.new()
	if cfg.load(TOKEN_FILE) != OK:
		return ""
	return String(cfg.get_value("server", "token", "")).strip_edges()


static func set_token(value: String) -> void:
	var cfg := ConfigFile.new()
	cfg.load(TOKEN_FILE)
	cfg.set_value("server", "token", value.strip_edges())
	var err := cfg.save(TOKEN_FILE)
	if err != OK:
		push_error("Ambar: could not write the token file (error %d)" % err)


## pref reads one browse preference, falling back to `fallback` when it was never set or the file
## is unreadable. Untyped on purpose: the same two functions carry ints and strings.
static func pref(key: String, fallback):
	var cfg := ConfigFile.new()
	if cfg.load(PREFS_FILE) != OK:
		return fallback
	return cfg.get_value("ui", key, fallback)


static func set_pref(key: String, value) -> void:
	var cfg := ConfigFile.new()
	cfg.load(PREFS_FILE)
	cfg.set_value("ui", key, value)
	var err := cfg.save(PREFS_FILE)
	if err != OK:
		push_error("Ambar: could not write %s (error %d)" % [PREFS_FILE, err])


## configured reports whether there is enough to try a request. Both halves are required: Ambar
## has no anonymous API, so a URL without a token only produces 401s.
static func configured() -> bool:
	return base_url() != "" and token() != ""


## token_file_path is shown in the UI so somebody can find, back up or delete it.
static func token_file_path() -> String:
	return ProjectSettings.globalize_path(TOKEN_FILE)
