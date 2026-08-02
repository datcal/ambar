@tool
extends Node
## Renders a model to a picture, using the renderer the plugin is already sitting inside.
##
## Ambar has no renderer and deliberately will not grow one: the specification keeps Blender optional, so a
## model's derive produces geometry and metadata and never an image. The consequence, on a real
## library, is 526 model groups of which 178 have a thumbnail — the ones somebody happened to open
## in the web viewer, which renders in three.js and posts the result back. Every other model
## is a blank tile, in the browser and in this plugin alike.
##
## The plugin's advantage is absurd once stated: it runs in Godot. `preview.glb` exists for every
## glTF and OBJ in the library, `GLTFDocument` reads it, and a `SubViewport` draws it. So the
## grid fills itself in as it is browsed, and each render is posted back to the server — where the
## upload endpoint refuses to overwrite a thumbnail that already exists, so the work happens once
## per model no matter how many people browse past it.
##
## What this cannot do is FBX: Godot has no runtime FBX importer either, and `preview.glb` is not
## written for a format Blender was needed to read in the first place. Those stay honest about
## needing Blender.

## The rendered side, in pixels. Matches what the web viewer uploads, and the server's cap is a
## megabyte — a 512² PNG of one model is tens of kilobytes.
const SIDE := 512

var _viewport: SubViewport
var _camera: Camera3D
var _busy := false


func _ready() -> void:
	if _viewport != null:
		return
	_viewport = SubViewport.new()
	_viewport.size = Vector2i(SIDE, SIDE)
	# Transparent, because a thumbnail sits on whatever background the grid has, and the server
	# rejects a blank image — an opaque grey square would pass that check while showing nothing.
	_viewport.transparent_bg = true
	# Its own world, or the model would be lit by (and visible to) whatever else is in the editor.
	_viewport.own_world_3d = true
	_viewport.render_target_update_mode = SubViewport.UPDATE_DISABLED
	add_child(_viewport)

	_camera = Camera3D.new()
	_camera.fov = 35.0 # long-ish lens: less perspective distortion on a boxy prop
	_viewport.add_child(_camera)

	# A key light from over the camera's shoulder plus flat ambient. Not a beautiful studio
	# setup — the job is "what is this object", and a face in total darkness fails that.
	var key := DirectionalLight3D.new()
	key.rotation_degrees = Vector3(-40, -35, 0)
	key.light_energy = 1.4
	_viewport.add_child(key)

	var fill := DirectionalLight3D.new()
	fill.rotation_degrees = Vector3(-15, 140, 0)
	fill.light_energy = 0.5
	_viewport.add_child(fill)

	var environment := Environment.new()
	environment.background_mode = Environment.BG_CANVAS
	environment.ambient_light_source = Environment.AMBIENT_SOURCE_COLOR
	environment.ambient_light_color = Color(0.6, 0.62, 0.68)
	environment.ambient_light_energy = 0.6
	var world := WorldEnvironment.new()
	world.environment = environment
	_viewport.add_child(world)


## busy reports whether a render is already running. One viewport, one camera, one model at a
## time: the caller queues rather than interleaving.
func busy() -> bool:
	return _busy


## render turns glTF/glb bytes into an Image, or null if they cannot be drawn.
##
## Awaits, so callers must await it too. A second caller waits its turn rather than being told
## no: returning null while busy makes the answer depend on timing, and the caller cannot tell
## "this model will not draw" from "something else was drawing" — the panel reported the first
## while the second was true.
func render(glb_bytes: PackedByteArray) -> Image:
	if glb_bytes.is_empty():
		return null
	while _busy:
		await RenderingServer.frame_post_draw
	_busy = true
	var image := await _render(glb_bytes)
	_busy = false
	return image


func _render(glb_bytes: PackedByteArray) -> Image:
	var document := GLTFDocument.new()
	var state := GLTFState.new()
	if document.append_from_buffer(glb_bytes, "", state) != OK:
		return null

	var scene: Node = document.generate_scene(state)
	if scene == null:
		return null
	_viewport.add_child(scene)

	var bounds := _bounds(scene)
	if bounds.size == Vector3.ZERO:
		# Geometry-free glTF: a scene graph with no meshes renders as nothing, and an empty
		# picture posted to the server would be rejected there anyway.
		_viewport.remove_child(scene)
		scene.queue_free()
		return null
	_frame(bounds)

	_viewport.render_target_update_mode = SubViewport.UPDATE_ONCE
	# Two frames, not one: the first is when the newly added meshes are actually drawn.
	await RenderingServer.frame_post_draw
	await RenderingServer.frame_post_draw

	var texture := _viewport.get_texture()
	var image: Image = texture.get_image() if texture != null else null

	_viewport.remove_child(scene)
	scene.queue_free()
	return image


## _bounds is the whole model's extent, in the scene's own space, so the camera can frame it.
func _bounds(node: Node) -> AABB:
	var total := AABB()
	var found := false
	for child in _visuals(node):
		# Annotated, not inferred: `_visuals` returns an untyped Array, and `var box :=
		# child.get_aabb()` is a *parse* error in GDScript — the one that failed the compile of
		# every script preloading it, which leaves the addon enabled and completely inert.
		var visual: VisualInstance3D = child
		var box: AABB = visual.get_aabb()
		var transformed: AABB = visual.global_transform * box
		if not found:
			total = transformed
			found = true
		else:
			total = total.merge(transformed)
	return total if found else AABB()


func _visuals(node: Node) -> Array:
	var out: Array = []
	if node is VisualInstance3D:
		out.append(node)
	for child in node.get_children():
		out.append_array(_visuals(child))
	return out


## _frame places the camera on a three-quarter view far enough back to hold the whole model.
##
## The same angle the web viewer opens on, so a model looks like itself in both places.
func _frame(bounds: AABB) -> void:
	var centre := bounds.get_center()
	var radius := maxf(bounds.size.length() * 0.5, 0.001)
	# 1.25 is margin: the bounding sphere touches the frame edge otherwise, and a model that
	# fills its thumbnail edge to edge reads as cropped.
	var distance := radius / tan(deg_to_rad(_camera.fov * 0.5)) * 1.25
	var direction := Vector3(0.7, 0.5, 1.0).normalized()
	_camera.position = centre + direction * distance
	_camera.look_at(centre, Vector3.UP)
	# Near/far around the object rather than the defaults, so a 0.1 m prop and a 200 m terrain
	# both survive depth precision.
	_camera.near = maxf(distance - radius * 2.0, distance * 0.01)
	_camera.far = distance + radius * 4.0
