// Grid thumbnails for 3D models, rendered here in the browser (M15).
//
// The server has no 3D renderer and Blender is optional (§6), so a model tile used to
// show an extension chip and nothing else. This island fills that gap: for each model
// tile without a thumbnail, load the model once off-screen, snapshot it, and hand the
// PNG back to the server, which re-encodes and caches it as an ordinary derivative.
// One render per model, ever.
//
// Rules it follows, because a grid must stay usable while this happens:
//
//   - One at a time, and only tiles that have scrolled into view.
//   - A small budget per page load. Browsing 500 models should not turn the tab into a
//     render farm; the rest get their turn on the next visit.
//   - Failure is silent per tile. A model the loader cannot read simply keeps its
//     extension chip.
//   - One WebGL context for everything. Contexts are a scarce browser resource.
//   - Nothing is uploaded until the model's textures have settled, and a texture that
//     never arrives is dropped rather than left as a placeholder. three.js renders a
//     mesh whose texture is missing as transparent black, so skipping either of these
//     produced a fully transparent snapshot — which is exactly what every .fbx tile in
//     this library got, and what the server now refuses.
//
// The drawing itself is exported as window.AmbarModelThumb.render, because the grid is
// not the only caller: the jobs page's "redraw model thumbnails" works through a queue of
// models with the same code, in the same tab (renderqueue.js).
(function () {
    "use strict";

    if (typeof THREE === "undefined") return;

    // Which renderer this is, sent with every upload (internal/server/modelthumb.go). 2
    // resolves textures by name through /assets/{id}/texture; the server lets it replace
    // a thumbnail drawn by an older one.
    var RENDERER = 2;

    // Fallback cap, used only when the browser has no IntersectionObserver and there is
    // therefore no way to tell what the reader has actually looked at. With one, every
    // tile scrolled past is rendered and this number is not consulted (M17).
    var BUDGET = 12;
    var SIZE = 512;

    var csrf = (document.querySelector('meta[name="csrf-token"]') || {}).content || "";

    var renderer = null;
    var scene = null;
    var camera = null;

    function setup() {
        var canvas = document.createElement("canvas");
        canvas.width = SIZE;
        canvas.height = SIZE;
        renderer = new THREE.WebGLRenderer({
            canvas: canvas,
            antialias: true,
            alpha: true,
            preserveDrawingBuffer: true,
        });
        renderer.setSize(SIZE, SIZE, false);

        scene = new THREE.Scene();
        scene.add(new THREE.HemisphereLight(0xffffff, 0x333333, 1.2));
        var key = new THREE.DirectionalLight(0xffffff, 0.9);
        key.position.set(2, 3, 2);
        scene.add(key);

        camera = new THREE.PerspectiveCamera(35, 1, 0.01, 1000);
        return true;
    }

    // Did the loader actually give us geometry? A loader that resolves with an empty
    // scene — an FBX whose meshes it could not read, a glTF with only lights — would
    // otherwise be snapshotted as a transparent square and uploaded as this asset's
    // thumbnail. That is exactly what happened to every .fbx in the library: a 550-byte
    // blank tile, and worse, a derive_state of "ok" that hid the real problem.
    function hasGeometry(object) {
        var vertices = 0;
        object.traverse(function (node) {
            var geometry = node.geometry;
            if (!geometry) return;
            var position = geometry.attributes && geometry.attributes.position;
            if (position && position.count) vertices += position.count;
        });
        return vertices > 0;
    }

    var TEXTURE_SLOTS = [
        "map", "specularMap", "emissiveMap", "normalMap", "bumpMap",
        "aoMap", "alphaMap", "metalnessMap", "roughnessMap", "envMap",
    ];

    function textureArrived(texture) {
        var image = texture && texture.image;
        if (!image) return false;
        if (typeof image.complete === "boolean" && !image.complete) return false;
        return (image.width || 0) > 0 || !!image.data;
    }

    // Clear texture slots whose image never arrived. Called only once fetching has
    // settled, so nothing still in flight is discarded. Without this the snapshot is a
    // transparent square: a missing texture multiplies the material down to rgba(0,0,0,0).
    //
    // Returns how many were dropped: the server records it, so a thumbnail drawn without
    // its textures is known to be one.
    function dropMissingTextures(object) {
        var dropped = 0;
        object.traverse(function (node) {
            if (!node.material) return;
            var mats = Array.isArray(node.material) ? node.material : [node.material];
            mats.forEach(function (m) {
                TEXTURE_SLOTS.forEach(function (slot) {
                    if (m[slot] && !textureArrived(m[slot])) {
                        m[slot] = null;
                        m.needsUpdate = true;
                        dropped += 1;
                    }
                });
            });
        });
        return dropped;
    }

    // Frame the model the way the detail viewer does: fit the bounding sphere, look
    // slightly down at it, so a tile reads as an object rather than as a silhouette.
    function frame(object) {
        var box = new THREE.Box3().setFromObject(object);
        if (box.isEmpty()) return false;

        var sphere = box.getBoundingSphere(new THREE.Sphere());
        var radius = sphere.radius || 1;
        var distance = radius / Math.sin((camera.fov * Math.PI) / 360);

        camera.position.set(
            sphere.center.x + distance * 0.6,
            sphere.center.y + distance * 0.5,
            sphere.center.z + distance * 0.75
        );
        camera.near = Math.max(distance / 100, 0.001);
        camera.far = distance * 10;
        camera.updateProjectionMatrix();
        camera.lookAt(sphere.center);
        return true;
    }

    function loaderFor(format, mtl, manager) {
        switch (format) {
            case "obj":
                return function (src, done, fail) {
                    var base = src.slice(0, src.lastIndexOf("/") + 1);
                    var load = function (materials) {
                        var loader = new THREE.OBJLoader(manager);
                        if (materials) loader.setMaterials(materials);
                        loader.load(src, done, undefined, fail);
                    };
                    if (mtl && typeof THREE.MTLLoader !== "undefined") {
                        new THREE.MTLLoader(manager).setPath(base).load(
                            mtl,
                            function (materials) {
                                materials.preload();
                                load(materials);
                            },
                            undefined,
                            function () { load(null); }
                        );
                    } else {
                        load(null);
                    }
                };
            case "fbx":
                if (typeof THREE.FBXLoader === "undefined") return null;
                return function (src, done, fail) {
                    new THREE.FBXLoader(manager).load(src, done, undefined, fail);
                };
            default:
                return function (src, done, fail) {
                    new THREE.GLTFLoader(manager).load(
                        src,
                        function (gltf) { done(gltf.scene); },
                        undefined,
                        fail
                    );
                };
        }
    }

    function upload(assetID, blob, untextured, done) {
        var url = "/assets/" + encodeURIComponent(assetID) + "/thumb";
        fetch(url, {
            method: "POST",
            headers: {
                "X-CSRF-Token": csrf,
                "Content-Type": "image/png",
                "X-Ambar-Renderer": String(RENDERER),
                "X-Ambar-Untextured": String(untextured),
            },
            body: blob,
            credentials: "same-origin",
        }).then(
            function (response) { done(response.ok); },
            function () { done(false); }
        );
    }

    function show(tile, assetID) {
        // Swap the chip for the freshly stored thumbnail without a reload.
        var img = document.createElement("img");
        img.src = "/assets/" + encodeURIComponent(assetID) + "/thumb";
        img.alt = "";
        img.loading = "lazy";
        if (tile.parentNode) tile.parentNode.replaceChild(img, tile);
    }

    var ready = false;

    // render draws one model off-screen and uploads the picture. item is
    // {asset, src, format, mtl}; callback(stored, untextured) is called exactly once.
    function render(item, callback) {
        if (!ready) ready = setup();
        var src = item.src;
        var assetID = item.asset;
        // A manager per model: it is how "have the textures finished?" is answered, and
        // FBXLoader in particular calls back long before its textures are in.
        var manager = new THREE.LoadingManager();
        // Textures are found by name rather than by the path the model carries
        // (modeltextures.js), so a thumbnail is as textured as the viewer.
        if (window.AmbarModelTextures && src) {
            manager.setURLModifier(window.AmbarModelTextures.modifierFor(src));
        }
        var load = loaderFor((item.format || "gltf").toLowerCase(), item.mtl, manager);

        var done = false;
        var dropped = 0;
        var finish = function (stored) {
            if (done) return;
            done = true;
            callback(!!stored, dropped);
        };
        if (!load || !src || !assetID) return finish(false);

        var loaded = null;
        var settled = false;

        // Snapshot only when the model and its fetches are both accounted for; whichever
        // arrives last triggers it.
        var snapshot = function () {
            if (done || !settled || !loaded) return;
            var object = loaded;
            try {
                if (!hasGeometry(object)) {
                    // An empty scene — a loader that read the file but found no meshes.
                    // Uploading a picture of it would replace an honest extension chip
                    // with a blank square.
                    return finish(false);
                }
                dropped = dropMissingTextures(object);
                scene.add(object);
                var framed = frame(object);
                if (framed) renderer.render(scene, camera);
                scene.remove(object);
                if (!framed) return finish(false);

                renderer.domElement.toBlob(function (blob) {
                    if (!blob) return finish(false);
                    upload(assetID, blob, dropped, finish);
                }, "image/png");
            } catch (e) {
                finish(false);
            }
        };

        manager.onLoad = function () { settled = true; snapshot(); };
        // A fetch that never completes must not hold the queue: take the picture with
        // whatever arrived, and dropMissingTextures keeps it from being blank.
        window.setTimeout(function () { settled = true; snapshot(); }, 10000);

        load(
            src,
            function (object) {
                loaded = object;
                snapshot();
            },
            function () {
                // Unreadable model: nothing to upload.
                finish(false);
            }
        );
    }

    window.AmbarModelThumb = { render: render, renderer: RENDERER };

    // --- the grid ---------------------------------------------------------------------

    var pending = Array.prototype.slice.call(
        document.querySelectorAll(".thumb-pending[data-model]")
    );
    if (!pending.length) return;

    var queue = [];
    var running = false;

    function next() {
        if (running) return;
        var tile = queue.shift();
        if (!tile) return;
        running = true;
        render(
            { asset: tile.dataset.asset, src: tile.dataset.model, format: tile.dataset.format, mtl: tile.dataset.mtl },
            function (stored) {
                if (stored) show(tile, tile.dataset.asset);
                running = false;
                // Yield to the browser between models so scrolling stays smooth.
                window.setTimeout(next, 50);
            }
        );
    }

    ready = setup();
    if (!ready) return;

    // Only what has been seen. An IntersectionObserver keeps a 500-model page from
    // rendering anything the user never scrolled to.
    //
    // M17: no per-page-load cap any more. There was one — twelve — and the arithmetic
    // was against it: 244 models in this library still had no thumbnail, a page shows a
    // hundred tiles, so filling one page took about twenty visits. "Çoğu model dosyasının
    // görüntüsü yok" was that cap, not a failure to render. Everything you actually
    // scroll past now gets its turn, still strictly one at a time through the single
    // WebGL context, so the queue is a background trickle rather than a burst.
    if (typeof IntersectionObserver === "undefined") {
        // No observer: fall back to a fixed slice, because without visibility
        // information "everything" would mean every tile on the page at once.
        queue = pending.slice(0, BUDGET);
        next();
        return;
    }

    var observer = new IntersectionObserver(
        function (entries) {
            entries.forEach(function (entry) {
                if (!entry.isIntersecting) return;
                observer.unobserve(entry.target);
                queue.push(entry.target);
                next();
            });
        },
        { rootMargin: "200px" }
    );
    pending.forEach(function (tile) { observer.observe(tile); });
})();
