@tool
extends RefCounted
## Bringing an asset into the project.
##
## The file lands at res://assets/<kind>/<pack-slug>/<filename>, the manifest records it, and the
## server is told — tolerating being offline, because the manifest is the source of truth to
## reconcile from later.
##
## What this deliberately does *not* do is write `.import` files by hand, which is what the
## previous version did. Godot owns those: it writes `source_file`, `dest_files` and a `[deps]`
## section keyed to its own version, and a partial hand-written one is either overwritten or an
## import error. The old code's own comment said the keys "must be verified in-editor", and they
## never were.
##
## Import *defaults* are the supported way to say "this project is pixel art", and they are one
## ProjectSettings key: `importer_defaults/texture`. apply_pixel_art_defaults() sets it once, for
## every future import, which is both more useful and less breakable than per-file surgery.

const Project := preload("res://addons/ambar/project.gd")

## import_asset downloads one asset and records it. cb receives (ok, message, res_path).
static func import_asset(api: RefCounted, a: Dictionary, cb: Callable) -> void:
	var asset_id := int(a.get("id", 0))
	if asset_id <= 0:
		cb.call(false, "that asset has no id", "")
		return

	var filename := String(a.get("filename", "asset.bin"))
	var kind := String(a.get("kind", "other"))
	var pack: Dictionary = a.get("pack", {}) if a.get("pack") is Dictionary else {}
	var slug := String(pack.get("slug", "pack"))

	var dir := "res://assets/%s/%s" % [kind, slug]
	var abs_dir := ProjectSettings.globalize_path(dir)
	var err := DirAccess.make_dir_recursive_absolute(abs_dir)
	if err != OK and not DirAccess.dir_exists_absolute(abs_dir):
		cb.call(false, "could not create %s (error %d)" % [dir, err], "")
		return

	var res_path := "%s/%s" % [dir, filename]
	var abs_dest := ProjectSettings.globalize_path(res_path)

	var expected := String(a.get("sha256", ""))
	api.download_file(asset_id, abs_dest, func(ok, result):
		if not ok:
			cb.call(false, str(result), "")
			return

		# What arrived must be what was advertised.
		#
		# This exists because it did not hold once: a project ended up with a file named
		# `3.png`, in a folder named after the pack `3.png` came from, whose bytes were
		# `Ship2_shadow1.png` from an entirely different pack — so the generated CREDITS.md
		# correctly credited a pack the person had never chosen, and did not credit the one
		# they thought they had used. Whatever produced that mismatch, the import is the place
		# it stops being invisible: provenance is the whole point of this application, and
		# recording it for bytes nobody verified is worse than failing.
		var landed := FileAccess.get_sha256(res_path)
		if expected != "" and landed != "" and landed != expected:
			DirAccess.remove_absolute(abs_dest)
			cb.call(false, ("the server sent different content than it listed for %s " +
				"(expected %s…, got %s…) — nothing was imported") % [
					filename, expected.substr(0, 8), landed.substr(0, 8)], "")
			return

		Project.record(asset_id, {
			"res_path": res_path,
			"sha256": String(a.get("sha256", "")),
			"filename": filename,
			"pack": String(pack.get("name", "")),
		})

		# Telling the server is what makes the library's "anything a project uses is never a removal
		# candidate" true, and what the credits file is built from. Failing is survivable: the
		# manifest above is committed, so a later reconcile can replay it.
		api.record_use(Project.uuid(), Project.name_hint(), asset_id, res_path,
			String(a.get("sha256", "")), func(sent, message):
				if sent:
					cb.call(true, "Imported %s" % res_path, res_path)
				else:
					cb.call(true, "Imported %s — the server was not told (%s)" % [res_path, message], res_path)
		)
	)


## update_asset re-downloads an asset the library has changed, over the copy already in the
## project.
##
## Deliberately the same res_path rather than a fresh one: the file is referenced by scenes and
## resources, and writing the new bytes where the old ones were is what makes the update invisible
## to everything that points at it. `use` is a row from /projects/{uuid}/uses.
static func update_asset(api: RefCounted, use: Dictionary, cb: Callable) -> void:
	var asset_id := int(use.get("asset_id", 0))
	var res_path := String(use.get("res_path", ""))
	if asset_id <= 0 or not res_path.begins_with("res://"):
		cb.call(false, "that entry has no asset id or no project path")
		return

	var abs_dest := ProjectSettings.globalize_path(res_path)
	api.download_file(asset_id, abs_dest, func(ok, result):
		if not ok:
			cb.call(false, str(result))
			return
		Project.record(asset_id, {
			"res_path": res_path,
			"sha256": String(use.get("sha256", "")),
			"filename": String(use.get("filename", "")),
			"pack": String(use.get("pack", "")),
		})
		# Tell the server the new hash too, or the row stays marked outdated for ever.
		api.record_use(Project.uuid(), Project.name_hint(), asset_id, res_path,
			String(use.get("sha256", "")), func(sent, message):
				if sent:
					cb.call(true, "Updated %s" % res_path)
				else:
					cb.call(true, "Updated %s — the server was not told (%s)" % [res_path, message])
		)
	)


## apply_pixel_art_defaults points this project's texture importer at settings that do not destroy
## pixel art: nearest filtering, no mipmaps, lossless.
##
## Project-wide and one-time, rather than per file. the specification calls bilinear-scaled pixel art "the single
## most annoying failure of every existing tool"; in Godot the equivalent mistake is the default
## linear filter, and it is a project setting, not a per-asset one.
static func apply_pixel_art_defaults() -> String:
	var defaults := {
		"compress/mode": 0,          # lossless: never crunch a sprite
		"mipmaps/generate": false,   # a 2D sprite has no use for them
		"detect_3d/compress_to": 0,  # stop Godot "helpfully" switching to VRAM compression
		"process/fix_alpha_border": true,
	}
	ProjectSettings.set_setting("importer_defaults/texture", defaults)

	# Filtering is a rendering default rather than an import one, and it is the setting that
	# actually decides whether a sprite looks like pixel art on screen.
	ProjectSettings.set_setting("rendering/textures/canvas_textures/default_texture_filter", 0)

	var err := ProjectSettings.save()
	if err != OK:
		return "could not save project settings (error %d)" % err
	return "Pixel-art import defaults set for this project. Re-import existing textures to apply them."
