// Scans camera frames with the jabstream wasm receiver off the main
// thread, the JAB Code peer of worker.js: symbol decoding is far
// heavier than a page can afford per camera frame.

const logging = new URLSearchParams(self.location.search).get('log') === '1';
function wlog(message) {
    if (!logging) return;
    const text = 'worker ' + String(message);
    console.debug('[serve]', text);
    try {
        fetch('/log', {
            method: 'POST',
            headers: { 'Content-Type': 'text/plain; charset=utf-8' },
            body: text,
        }).catch(() => {});
    } catch {}
}

const ready = (async () => {
    const started = performance.now();
    importScripts('/wasm_exec.js');
    const go = new Go();
    const r = await WebAssembly.instantiateStreaming(fetch('/jabstream.wasm'), go.importObject);
    go.run(r.instance);
    wlog('wasm ready ms=' + Math.round(performance.now() - started));
})();

let scans = 0;
let hits = 0;
let misses = 0;
let decodeMillis = 0;
let lastReport = performance.now();
function reportStats(force) {
    if (!logging) return;
    const now = performance.now();
    if (!force && now - lastReport < 5000) return;
    const average = scans ? Math.round(decodeMillis / scans) : 0;
    wlog('scans=' + scans + ' hits=' + hits + ' misses=' + misses + ' avg_ms=' + average);
    lastReport = now;
}

onmessage = async e => {
    try {
        await ready;
    } catch (err) {
        wlog('wasm load failed ' + err.name + ': ' + err.message);
        postMessage({ error: 'decoder failed to load: ' + err.message, recoverable: false });
        return;
    }
    const { width, height, buffer } = e.data;
    let result;
    const started = performance.now();
    try {
        result = jabstreamScanFrame({ width, height, data: new Uint8Array(buffer) });
    } catch (err) {
        wlog('decode failed ' + err.name + ': ' + err.message);
        postMessage({ error: 'decode error: ' + err.message, recoverable: false });
        return;
    }
    scans++;
    decodeMillis += performance.now() - started;
    if (result) hits++;
    else misses++;
    reportStats(Boolean(result && result.done));
    if (!result) {
        postMessage(null);
        return;
    }
    if (result.error) {
        postMessage({ error: String(result.error), recoverable: Boolean(result.recoverable) });
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
