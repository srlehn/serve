// Scans camera frames with the qrstream wasm receiver off the main
// thread: a slow decode (a blurry frame costs the full detection
// ladder) must not freeze the page or the camera preview.

const ready = (async () => {
    importScripts('/wasm_exec.js');
    const go = new Go();
    const r = await WebAssembly.instantiateStreaming(fetch('/qrstream.wasm'), go.importObject);
    go.run(r.instance);
})();

onmessage = async e => {
    try {
        await ready;
    } catch (err) {
        postMessage({ error: 'decoder failed to load: ' + err.message });
        return;
    }
    const { width, height, buffer } = e.data;
    let result;
    try {
        result = qrstreamScanFrame({ width, height, data: new Uint8Array(buffer) });
    } catch (err) {
        postMessage({ error: 'decode error: ' + err.message });
        return;
    }
    if (!result) {
        postMessage(null);
        return;
    }
    if (result.sameAsLast) {
        postMessage({ sameAsLast: true });
        return;
    }
    const out = {
        fileID: result.fileID, have: result.have, total: result.total, done: result.done,
    };
    if (result.done) {
        out.name = result.name;
        out.data = result.data;
    }
    postMessage(out);
};
