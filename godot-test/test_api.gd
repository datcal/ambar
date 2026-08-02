extends SceneTree
## Drives the plugin's API client against a running server, headlessly.
##
## `--headless --editor --quit` proves the scripts parse; this proves they *work* — that the
## grouped search comes back grouped, that the pager numbers are there, that a detail request
## carries variants and a licence, and that a thumbnail decodes into an Image. None of it needs a
## display, and all of it is what the panel is about to do for real.
##
##   godot --headless --script test_api.gd --path <project>

const Api := preload("res://addons/ambar/api.gd")
const Config := preload("res://addons/ambar/config.gd")

var _failures := 0
var _host: Node


# _initialize, not _init: the SceneTree exists by the time the main loop initialises it, and
# `root` is null before that — which is a hang rather than an error, because the first request
# never starts and the harness waits for a callback that cannot come.
func _initialize() -> void:
	_host = Node.new()
	root.add_child(_host)
	_run()


## Do not override _process here. SceneTree's own implementation is what walks the node tree, a
## GDScript override replaces it rather than extending it, and unprocessed HTTPRequest nodes never
## complete — which reads exactly like an unreachable server and is not.

func _api() -> RefCounted:
	return Api.new(Config.base_url(), Config.token(), _host)


func check(label: String, ok: bool, detail: String = "") -> void:
	if ok:
		print("  ok    %s%s" % [label, ("  " + detail) if detail != "" else ""])
	else:
		_failures += 1
		print("  FAIL  %s  %s" % [label, detail])


## await_call runs one API method and returns [ok, result].
##
## The callback *appends* rather than assigning. GDScript lambdas capture locals by value, so
## `out = [ok, result]` inside one rebinds a copy and the caller waits forever on an array that
## never fills; appending mutates the array both sides are holding. This cost an hour, and it is
## the same shape of trap as the parse error that once made the plugin silently do nothing.
func await_call(method: Callable) -> Array:
	var out: Array = []
	method.call(func(ok, result): out.append([ok, result]))
	var frames := 0
	while out.is_empty():
		await process_frame
		frames += 1
		if frames > 2000:
			return [false, "no answer from the server"]
	return out[0]


func _run() -> void:
	# The root Window is not itself in the tree until the first frame, so nothing parented under
	# it is either — and an HTTPRequest outside the tree refuses to run. api.gd says so in as many
	# words rather than blaming the URL, which is how this took a minute to find instead of an
	# afternoon.
	await process_frame

	print("ambar plugin — API drive against %s" % Config.base_url())

	if Config.base_url() == "" or Config.token() == "":
		print("  FAIL  not configured: write res://ambar.cfg and user://ambar_token.cfg first")
		quit(1)
		return

	# ping
	var ping: Array = await await_call(func(cb): _api().ping(cb))
	check("ping", ping[0], str(ping[1]).substr(0, 80))

	# grouped, sorted, paged search
	var first: Array = await await_call(func(cb): _api().search("", "any", "name", 1, 12, cb))
	check("search page 1", first[0], "")
	var page1: Dictionary = first[1] if first[0] and first[1] is Dictionary else {}
	var assets1: Array = page1.get("assets", [])
	check("grouped", bool(page1.get("grouped", false)), "total=%d" % int(page1.get("total", 0)))
	check("page size honoured", assets1.size() == 12, "got %d rows" % assets1.size())
	check("pages counted", int(page1.get("pages", 0)) > 1, "pages=%d" % int(page1.get("pages", 0)))
	check("page numbers", not (page1.get("page_numbers", []) as Array).is_empty(),
		str(page1.get("page_numbers", [])))
	check("no cursor offered", String(page1.get("next_cursor", "x")) == "", "")
	check("range reported", int(page1.get("last_shown", 0)) == 12,
		"%d–%d of %d" % [int(page1.get("first_shown", 0)), int(page1.get("last_shown", 0)), int(page1.get("total", 0))])

	# page 2 must be different rows
	var second: Array = await await_call(func(cb): _api().search("", "any", "name", 2, 12, cb))
	var page2: Dictionary = second[1] if second[0] and second[1] is Dictionary else {}
	var assets2: Array = page2.get("assets", [])
	var same := assets1.size() > 0 and assets2.size() > 0 and int(assets1[0].get("id", 0)) == int(assets2[0].get("id", -1))
	check("page 2 differs", not same, "first ids %d vs %d" % [
		int(assets1[0].get("id", 0)) if assets1.size() > 0 else -1,
		int(assets2[0].get("id", 0)) if assets2.size() > 0 else -1])

	# sort orders actually reorder
	var desc: Array = await await_call(func(cb): _api().search("", "any", "name-desc", 1, 12, cb))
	var descending: Dictionary = desc[1] if desc[0] and desc[1] is Dictionary else {}
	var desc_assets: Array = descending.get("assets", [])
	check("sort reverses", desc_assets.size() > 0 and assets1.size() > 0
		and String(desc_assets[0].get("filename", "")) != String(assets1[0].get("filename", "")),
		"%s vs %s" % [
			String(assets1[0].get("filename", "")) if assets1.size() > 0 else "-",
			String(desc_assets[0].get("filename", "")) if desc_assets.size() > 0 else "-"])

	# the sort list the toolbar renders
	var sorts: Array = await await_call(func(cb): _api().sorts(cb))
	var sort_list: Array = (sorts[1] as Dictionary).get("sorts", []) if sorts[0] and sorts[1] is Dictionary else []
	check("sort list", sort_list.size() >= 9, "%d orders" % sort_list.size())

	if assets1.is_empty():
		print("no assets — nothing further to check")
		quit(1 if _failures > 0 else 0)
		return

	# detail: tags, variants, licence, in one request
	var asset_id := int(assets1[0].get("id", 0))
	var detail: Array = await await_call(func(cb): _api().asset(asset_id, cb))
	var body: Dictionary = detail[1] if detail[0] and detail[1] is Dictionary else {}
	check("asset detail", detail[0], String((body.get("asset", {}) as Dictionary).get("filename", "")))
	check("detail has tags array", body.has("tags"), str(body.get("tags", [])).substr(0, 60))
	check("detail has provenance", body.has("provenance"), str(body.get("provenance", {})).substr(0, 80))

	# a multi-format asset, to prove variants come back
	var multi: Array = await await_call(func(cb): _api().search("psd", "any", "name", 1, 20, cb))
	var multi_page: Dictionary = multi[1] if multi[0] and multi[1] is Dictionary else {}
	for row in (multi_page.get("assets", []) as Array):
		if int(row.get("variant_count", 1)) > 1:
			var vd: Array = await await_call(func(cb): _api().asset(int(row.get("id", 0)), cb))
			var vbody: Dictionary = vd[1] if vd[0] and vd[1] is Dictionary else {}
			var variants: Array = vbody.get("variants", [])
			var exts: Array = []
			for v in variants:
				exts.append(String(v.get("ext", "")))
			check("variants listed", variants.size() == int(row.get("variant_count", 1)),
				"%s → %s" % [String(row.get("filename", "")), str(exts)])
			break

	# thumbnails and the full-size preview both decode
	var api: RefCounted = _api()
	var thumb: Array = await await_call(func(cb): api.fetch_bytes(api.thumb_url(asset_id), cb))
	check("thumbnail fetched", thumb[0], "%d bytes" % (thumb[1] as PackedByteArray).size())
	check("thumbnail decodes", _decodes(thumb[1]), _dimensions(thumb[1]))

	var api2: RefCounted = _api()
	var preview: Array = await await_call(func(cb): api2.fetch_bytes(api2.preview_url(asset_id), cb))
	check("preview fetched", preview[0], "%d bytes" % (preview[1] as PackedByteArray).size())
	check("preview decodes", _decodes(preview[1]), _dimensions(preview[1]))

	# a bad token still explains itself
	var bad := Api.new(Config.base_url(), "ambar_not_a_real_token", _host)
	var refused: Array = await await_call(func(cb): bad.search("", "any", "name", 1, 5, cb))
	check("bad token refused", not refused[0], str(refused[1]))

	print("%s (%d failure%s)" % ["FAILED" if _failures > 0 else "PASSED", _failures, "" if _failures == 1 else "s"])
	quit(1 if _failures > 0 else 0)


func _decodes(bytes: PackedByteArray) -> bool:
	var image := Image.new()
	if image.load_webp_from_buffer(bytes) == OK:
		return true
	return image.load_png_from_buffer(bytes) == OK


func _dimensions(bytes: PackedByteArray) -> String:
	var image := Image.new()
	if image.load_webp_from_buffer(bytes) != OK and image.load_png_from_buffer(bytes) != OK:
		return "undecodable"
	return "%d×%d" % [image.get_width(), image.get_height()]
