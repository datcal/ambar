@tool
extends RefCounted
## Thin async client for the Ambar §10 API. Every call takes a Callable that
## receives (ok: bool, result). The base URL and bearer token come from the
## plugin's Editor Settings.

var _base_url: String
var _token: String
var _tree: SceneTree


func _init(base_url: String, token: String, tree: SceneTree) -> void:
	_base_url = base_url
	_token = token
	_tree = tree


func _headers() -> PackedStringArray:
	return PackedStringArray([
		"Authorization: Bearer " + _token,
		"Accept: application/json",
	])


## search hits GET /api/v1/search and calls back with the parsed JSON.
func search(query: String, kind: String, cb: Callable) -> void:
	var url := _base_url + "/api/v1/search?q=" + query.uri_encode()
	if kind != "":
		url += "&kind=" + kind.uri_encode()
	_json_get(url, cb)


## download_file fetches an asset's original bytes to an absolute path. Supports
## large files via HTTPRequest's built-in download-to-file.
func download_file(asset_id: int, abs_dest: String, cb: Callable) -> void:
	var req := HTTPRequest.new()
	req.download_file = abs_dest
	_tree.root.add_child(req)
	req.request_completed.connect(func(result, code, _h, _b):
		req.queue_free()
		cb.call(result == HTTPRequest.RESULT_SUCCESS and code == 200, abs_dest)
	)
	var err := req.request(_base_url + "/api/v1/assets/%d/file" % asset_id, _headers())
	if err != OK:
		req.queue_free()
		cb.call(false, "request failed: %d" % err)


## record_use POSTs an import to the project (§10). Tolerates being offline: the
## caller queues on failure.
func record_use(project_uuid: String, project_name: String, asset_id: int, res_path: String, sha256: String, cb: Callable) -> void:
	var body := JSON.stringify({
		"asset_id": asset_id, "res_path": res_path,
		"sha256": sha256, "project_name": project_name,
	})
	var req := HTTPRequest.new()
	_tree.root.add_child(req)
	req.request_completed.connect(func(result, code, _h, _b):
		req.queue_free()
		cb.call(result == HTTPRequest.RESULT_SUCCESS and code == 201, code)
	)
	var headers := _headers()
	headers.append("Content-Type: application/json")
	var url := _base_url + "/api/v1/projects/" + project_uuid.uri_encode() + "/uses"
	if req.request(url, headers, HTTPClient.METHOD_POST, body) != OK:
		req.queue_free()
		cb.call(false, "request failed")


func _json_get(url: String, cb: Callable) -> void:
	var req := HTTPRequest.new()
	_tree.root.add_child(req)
	req.request_completed.connect(func(result, code, _h, body):
		req.queue_free()
		if result != HTTPRequest.RESULT_SUCCESS or code != 200:
			cb.call(false, "HTTP %d" % code)
			return
		var parsed = JSON.parse_string(body.get_string_from_utf8())
		cb.call(parsed != null, parsed)
	)
	if req.request(url, _headers()) != OK:
		req.queue_free()
		cb.call(false, "request failed")
