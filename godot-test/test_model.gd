extends SceneTree
## Renders models the server has no picture of, and posts them back.
##
## This is the pass that proves the plugin can do the one thing the server cannot: Ambar has no
## renderer and the specification keeps Blender optional, so a model's derive writes a normalised preview.glb and
## never an image. Godot reads that glb, so the plugin fills the gap — once per model, for
## everybody, because the upload endpoint refuses to overwrite a thumbnail that already exists.
##
##   godot --script test_model.gd --path <project>
##
## Needs a display: it renders. Nothing else in the suite does.

const Api := preload("res://addons/ambar/api.gd")
const Config := preload("res://addons/ambar/config.gd")
const ModelRender := preload("res://addons/ambar/model_render.gd")

var _failures := 0
var _host: Node
var _renderer: Node


func _initialize() -> void:
	_host = Node.new()
	root.add_child(_host)
	_renderer = ModelRender.new()
	root.add_child(_renderer)
	_run()


func _api() -> RefCounted:
	return Api.new(Config.base_url(), Config.token(), _host)


func check(label: String, ok: bool, detail: String = "") -> void:
	if ok:
		print("  ok    %s%s" % [label, ("  " + detail) if detail != "" else ""])
	else:
		_failures += 1
		print("  FAIL  %s  %s" % [label, detail])


func await_call(method: Callable) -> Array:
	var out: Array = []
	method.call(func(ok, result): out.append([ok, result]))
	var frames := 0
	while out.is_empty():
		await process_frame
		frames += 1
		if frames > 3000:
			return [false, "no answer from the server"]
	return out[0]


func _run() -> void:
	await process_frame
	print("ambar plugin — model rendering against %s" % Config.base_url())

	var found: Array = await await_call(func(cb): _api().search("", "model", "name", 1, 40, cb))
	var page: Dictionary = found[1] if found[0] and found[1] is Dictionary else {}
	var models: Array = page.get("assets", [])
	check("model search", found[0], "%d of %d model groups" % [models.size(), int(page.get("total", 0))])
	if models.is_empty():
		print("no models in this library — nothing to render")
		quit(1 if _failures > 0 else 0)
		return

	# One with no thumbnail on the server yet: the case this whole path exists for.
	var target: Dictionary = {}
	var already: Dictionary = {}
	for row in models:
		var api: RefCounted = _api()
		var thumb: Array = await await_call(func(cb): api.fetch_bytes(api.thumb_url(int(row.get("id", 0))), cb))
		if thumb[0] and not (thumb[1] as PackedByteArray).is_empty():
			if already.is_empty():
				already = row
		elif target.is_empty():
			target = row
		if not target.is_empty() and not already.is_empty():
			break

	check("found a model with no picture", not target.is_empty(),
		String(target.get("filename", "—")))
	if target.is_empty():
		print("every model already has a thumbnail; nothing left to prove here")
		quit(1 if _failures > 0 else 0)
		return

	var asset_id := int(target.get("id", 0))
	var links: Dictionary = target.get("links", {}) if target.get("links") is Dictionary else {}
	check("server offers a preview.glb", links.has("preview_glb"), str(links.keys()))

	var api2: RefCounted = _api()
	var glb: Array = await await_call(func(cb): api2.fetch_bytes(api2.preview_glb_url(asset_id), cb))
	check("preview.glb fetched", glb[0], "%d bytes" % (glb[1] as PackedByteArray).size())

	var image: Image = await _renderer.render(glb[1])
	check("rendered", image != null,
		"%d×%d" % [image.get_width(), image.get_height()] if image != null else "no image")
	if image == null:
		quit(1)
		return

	# A render of nothing is a transparent square, and the server rejects those — for good
	# reason, since it would become this model's picture for ever.
	var opaque := 0
	for y in range(0, image.get_height(), 4):
		for x in range(0, image.get_width(), 4):
			if image.get_pixel(x, y).a > 0.1:
				opaque += 1
	check("the render is not blank", opaque > 20, "%d sampled pixels drawn" % opaque)

	var png := image.save_png_to_buffer()
	var api3: RefCounted = _api()
	var stored: Array = await await_call(func(cb): api3.upload_thumb(asset_id, png, cb))
	check("stored on the server", stored[0], "%d KB · %s" % [png.size() / 1024, str(stored[1])])

	# And now every client gets it from the server rather than drawing it again.
	var api4: RefCounted = _api()
	var back: Array = await await_call(func(cb): api4.fetch_bytes(api4.thumb_url(asset_id), cb))
	check("served back as a thumbnail", back[0] and not (back[1] as PackedByteArray).is_empty(),
		"%d bytes" % (back[1] as PackedByteArray).size())

	# Rendering it again must not overwrite what is there: the endpoint answers "already had
	# one", which is what makes this safe to do optimistically from every client.
	var api5: RefCounted = _api()
	var again: Array = await await_call(func(cb): api5.upload_thumb(asset_id, png, cb))
	check("a second render is refused, not duplicated", again[0], str(again[1]))

	print("%s (%d failure%s)" % ["FAILED" if _failures > 0 else "PASSED", _failures, "" if _failures == 1 else "s"])
	quit(1 if _failures > 0 else 0)
