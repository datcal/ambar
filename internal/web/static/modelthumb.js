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
(function () {
    "use strict";

    var pending = Array.prototype.slice.call(
        document.querySelectorAll(".thumb-pending[data-model]")
    );
    if (!pending.length || typeof THREE === "undefined") return;

    // How many to render per page load. Enough that a page of models fills in while
    // you look at it, small enough that nothing stutters.
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

    function loaderFor(format, tile) {
        switch (format) {
            case "obj":
                return function (src, done, fail) {
                    var mtl = tile.dataset.mtl;
                    var base = src.slice(0, src.lastIndexOf("/") + 1);
                    var load = function (materials) {
                        var loader = new THREE.OBJLoader();
                        if (materials) loader.setMaterials(materials);
                        loader.load(src, done, undefined, fail);
                    };
                    if (mtl && typeof THREE.MTLLoader !== "undefined") {
                        new THREE.MTLLoader().setPath(base).load(
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
                    new THREE.FBXLoader().load(src, done, undefined, fail);
                };
            default:
                return function (src, done, fail) {
                    new THREE.GLTFLoader().load(
                        src,
                        function (gltf) { done(gltf.scene); },
                        undefined,
                        fail
                    );
                };
        }
    }

    function upload(assetID, blob, done) {
        var url = "/assets/" + encodeURIComponent(assetID) + "/thumb";
        fetch(url, {
            method: "POST",
            headers: { "X-CSRF-Token": csrf, "Content-Type": "image/png" },
            body: blob,
            credentials: "same-origin",
        }).then(
            function () { done(true); },
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

    var queue = [];
    var running = false;

    function next() {
        if (running) return;
        var tile = queue.shift();
        if (!tile) return;
        running = true;

        var src = tile.dataset.model;
        var assetID = tile.dataset.asset;
        var load = loaderFor((tile.dataset.format || "gltf").toLowerCase(), tile);

        var finish = function () {
            running = false;
            // Yield to the browser between models so scrolling stays smooth.
            window.setTimeout(next, 50);
        };
        if (!load || !src || !assetID) return finish();

        load(
            src,
            function (object) {
                try {
                    scene.add(object);
                    var framed = frame(object);
                    if (framed) renderer.render(scene, camera);
                    scene.remove(object);
                    if (!framed) return finish();

                    renderer.domElement.toBlob(function (blob) {
                        if (!blob) return finish();
                        upload(assetID, blob, function (ok) {
                            if (ok) show(tile, assetID);
                            finish();
                        });
                    }, "image/png");
                } catch (e) {
                    finish();
                }
            },
            function () {
                // Unreadable model: leave the chip alone and move on.
                finish();
            }
        );
    }

    if (!setup()) return;

    // Only what has been seen. An IntersectionObserver keeps a 500-model page from
    // rendering anything the user never scrolled to.
    var budget = BUDGET;
    if (typeof IntersectionObserver === "undefined") {
        queue = pending.slice(0, budget);
        next();
        return;
    }

    var observer = new IntersectionObserver(
        function (entries) {
            entries.forEach(function (entry) {
                if (!entry.isIntersecting || budget <= 0) return;
                observer.unobserve(entry.target);
                budget -= 1;
                queue.push(entry.target);
                next();
            });
        },
        { rootMargin: "200px" }
    );
    pending.forEach(function (tile) { observer.observe(tile); });
})();
