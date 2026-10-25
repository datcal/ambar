// Where a model's textures are fetched from (the 3D viewer and the grid's thumbnail
// renderer).
//
// A model names its textures however its author had them: `..\Textures\Tree.psd`, an
// absolute Windows path, a working file that was never shipped. Left alone, a loader
// asks for that path beside the model — and a browser resolves the `..` right out of
// /assets/{id}/file/ before the server sees it. So every image a loader asks for is sent,
// as the model named it, to /assets/{id}/texture, which finds the file that was meant
// (internal/server/textures.go). Everything else — an .obj's .mtl, a .gltf's .bin — still
// loads from beside the model, unchanged.
(function () {
    "use strict";

    var IMAGE = /\.(png|jpe?g|webp|gif|bmp|tga|psd|tiff?|dds)$/i;

    // modifierFor returns a LoadingManager URL modifier for the model at modelURL, a
    // /assets/{id}/file/<name> URL.
    function modifierFor(modelURL) {
        var base = modelURL.slice(0, modelURL.lastIndexOf("/") + 1);
        var match = /^(\/assets\/[^\/]+\/)file\/$/.exec(base);
        if (!match) return function (url) { return url; };
        var assetBase = match[1];

        return function (url) {
            if (url.indexOf(base) !== 0 || url === modelURL) return url;
            var ref = url.slice(base.length);
            var bare = ref.split("?")[0];
            if (!IMAGE.test(bare)) return url;
            var named;
            try {
                named = decodeURIComponent(bare);
            } catch (e) {
                named = bare;
            }
            return assetBase + "texture?ref=" + encodeURIComponent(named.replace(/\\/g, "/"));
        };
    }

    window.AmbarModelTextures = { modifierFor: modifierFor };
})();
