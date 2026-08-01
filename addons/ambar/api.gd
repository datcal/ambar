@tool
extends RefCounted
## Thin async client for the Ambar §10 API. Every call takes a Callable that receives
## (ok: bool, result) — parsed JSON on success, a *human* message on failure.
##
## The messages matter more than they look. The previous version reported "HTTP 0" for a server it
## could not reach and "HTTP 401" for a missing token, and showed both in a dock nobody had open —
## which is how "the plugin does nothing" happens. Every failure here comes back as a sentence
## that names the likely cause.

var _base_url: String
var _token: String
var _parent: Node # requests are parented here; an HTTPRequest must be in the tree


func _init(base_url: String, token: String, parent: Node) -> void:
	_base_url = base_url.rstrip("/")
	_token = token
	_parent = parent


func _headers() -> PackedStringArray:
	return PackedStringArray([
		"Authorization: Bearer " + _token,
		"Accept: application/json",
	])


## ping checks the URL and the token together, because that is the question being asked: "can I
## talk to the library". Reports what came back rather than a boolean.
func ping(cb: Callable) -> void:
	# /api/v1/ping, not /api/v1/healthz: the latter is session-authed for the browser and answers
	# 401 to a valid API token, which is a spectacularly misleading thing to show somebody who has
	# just pasted one.
	_json_get(_base_url + "/api/v1/ping", cb)


## search hits GET /api/v1/search. `after` is the cursor from a previous page, or "".
func search(query: String, kind: String, after: String, cb: Callable) -> void:
	var url := _base_url + "/api/v1/search?q=" + query.uri_encode()
	if kind != "" and kind != "any":
		url += "&kind=" + kind.uri_encode()
	if after != "":
		url += "&cursor=" + after.uri_encode()
	_json_get(url, cb)


## thumb_url is where a tile's picture comes from. No token in the URL — the request carries the
## header — so it is safe to log or paste.
func thumb_url(asset_id: int) -> String:
	return _base_url + "/api/v1/assets/%d/thumb" % asset_id


## fetch_bytes downloads to memory, for thumbnails.
func fetch_bytes(url: String, cb: Callable) -> void:
	var req := HTTPRequest.new()
	_parent.add_child(req)
	req.request_completed.connect(func(result, code, _h, body):
		req.queue_free()
		cb.call(result == HTTPRequest.RESULT_SUCCESS and code == 200, body)
	)
	if req.request(url, _headers()) != OK:
		req.queue_free()
		cb.call(false, PackedByteArray())


## download_file fetches an asset's original bytes to an absolute path, streaming to disk so a
## 200 MB model never goes through memory.
func download_file(asset_id: int, abs_dest: String, cb: Callable) -> void:
	var req := HTTPRequest.new()
	req.download_file = abs_dest
	_parent.add_child(req)
	req.request_completed.connect(func(result, code, _h, _b):
		req.queue_free()
		if result != HTTPRequest.RESULT_SUCCESS:
			cb.call(false, _transport_message(result))
			return
		if code != 200:
			cb.call(false, _status_message(code))
			return
		cb.call(true, abs_dest)
	)
	var err := req.request(_base_url + "/api/v1/assets/%d/file" % asset_id, _headers())
	if err != OK:
		req.queue_free()
		cb.call(false, "could not start the download (error %d)" % err)


## record_use POSTs an import to the project (§10). Tolerates being offline: the caller keeps the
## manifest, which is the source of truth to reconcile from later.
func record_use(project_uuid: String, project_name: String, asset_id: int, res_path: String, sha256: String, cb: Callable) -> void:
	var body := JSON.stringify({
		"asset_id": asset_id, "res_path": res_path,
		"sha256": sha256, "project_name": project_name,
	})
	var req := HTTPRequest.new()
	_parent.add_child(req)
	req.request_completed.connect(func(result, code, _h, _b):
		req.queue_free()
		if result != HTTPRequest.RESULT_SUCCESS:
			cb.call(false, _transport_message(result))
			return
		cb.call(code == 201 or code == 200, _status_message(code))
	)
	var headers := _headers()
	headers.append("Content-Type: application/json")
	var url := _base_url + "/api/v1/projects/" + project_uuid.uri_encode() + "/uses"
	if req.request(url, headers, HTTPClient.METHOD_POST, body) != OK:
		req.queue_free()
		cb.call(false, "could not send the request")


func _json_get(url: String, cb: Callable) -> void:
	if _base_url == "":
		cb.call(false, "no server URL configured")
		return
	var req := HTTPRequest.new()
	_parent.add_child(req)
	req.request_completed.connect(func(result, code, _h, body):
		req.queue_free()
		if result != HTTPRequest.RESULT_SUCCESS:
			cb.call(false, _transport_message(result))
			return
		if code != 200:
			cb.call(false, _status_message(code))
			return
		var parsed = JSON.parse_string(body.get_string_from_utf8())
		if parsed == null:
			cb.call(false, "the server answered with something that is not JSON")
			return
		cb.call(true, parsed)
	)
	if not req.is_inside_tree():
		# HTTPRequest refuses to run outside the tree, and the message it produces
		# ("ERR_UNCONFIGURED") explains nothing. This is a programming error, not a user one.
		req.queue_free()
		cb.call(false, "internal: the request was started before the panel was in the scene tree")
		return
	if req.request(url, _headers()) != OK:
		req.queue_free()
		cb.call(false, "could not send the request — is the URL a valid http:// address?")


## _transport_message explains a failure that never reached the server.
func _transport_message(result: int) -> String:
	match result:
		HTTPRequest.RESULT_CANT_CONNECT:
			return "cannot connect — check the address, and that Ambar is running"
		HTTPRequest.RESULT_CANT_RESOLVE:
			return "cannot resolve that hostname from this machine"
		HTTPRequest.RESULT_CONNECTION_ERROR:
			return "the connection dropped"
		HTTPRequest.RESULT_TIMEOUT:
			return "timed out"
		HTTPRequest.RESULT_TLS_HANDSHAKE_ERROR:
			return "TLS failed — try http:// if Ambar is on your LAN without a certificate"
		_:
			return "the request failed (transport code %d)" % result


## _status_message explains an HTTP status in terms of what to do about it.
func _status_message(code: int) -> String:
	match code:
		200, 201:
			return "ok"
		401:
			return "unauthorised — the API token is missing or wrong (Settings → API tokens in Ambar)"
		403:
			return "forbidden — the token exists but is not allowed to do that"
		404:
			return "not found — check that the server URL points at Ambar's root"
		_:
			return "the server answered %d" % code
