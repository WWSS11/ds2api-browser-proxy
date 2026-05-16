package browserproxy

import (
	"fmt"
	"strings"
)

var injectionScript string

func init() {
	injectionScript = buildInjectionScript()
}

func GetInjectionScript() (string, error) {
	if injectionScript == "" {
		return "", fmt.Errorf("[injector] script not initialized")
	}
	return injectionScript, nil
}

func GetInjectionScriptRaw() string {
	return injectionScript
}

func buildInjectionScript() string {
	return `(function(){
    if (window.__ds2api_injected) return;
    window.__ds2api_injected = true;

    window.__ds2api_chunks = [];
    window.__ds2api_error = null;
    window.__ds2api_done = false;
    window.__ds2api_status = 'idle';
    window.__ds2api_debug = [];
    window.__ds2api_all_urls = [];

    function ds2apiLog(msg) {
        window.__ds2api_debug.push('[' + new Date().toISOString().substr(11,12) + '] ' + msg);
        if (window.__ds2api_debug.length > 100) window.__ds2api_debug.shift();
        console.log('[ds2api] ' + msg);
    }

    function isCompletionUrl(url) {
        if (!url) return false;
        return url.indexOf('/chat/completion') !== -1 ||
               url.indexOf('/chat_completion') !== -1;
    }

    function isChatUrl(url) {
        if (!url) return false;
        return isCompletionUrl(url) ||
               url.indexOf('/api/chat') !== -1 ||
               url.indexOf('/v1/chat') !== -1;
    }

    function logUrl(source, url) {
        if (url) {
            window.__ds2api_all_urls.push(source + ':' + url);
            if (window.__ds2api_all_urls.length > 200) window.__ds2api_all_urls.shift();
        }
    }

    function startStreamCapture() {
        window.__ds2api_error = null;
        window.__ds2api_done = false;
        window.__ds2api_status = 'streaming';
        window.__ds2api_chunks = [];
    }

    var originalFetch = window.fetch;
    window.fetch = function(...args){
        var url = typeof args[0] === 'string' ? args[0] : (args[0] ? args[0].url : '');
        var isCompletion = isCompletionUrl(url);
        var isChat = isChatUrl(url);
        logUrl('fetch', url);
        ds2apiLog('fetch: ' + (url || 'no-url').substring(0, 120) + ' completion=' + isCompletion + ' chat=' + isChat);

        if (!isCompletion) {
            return originalFetch.apply(this, args);
        }

        ds2apiLog('INTERCEPTING fetch completion: ' + url);
        startStreamCapture();

        var promise = originalFetch.apply(this, args);

        promise.then(function(response){
            ds2apiLog('fetch resp status=' + response.status + ' body=' + !!(response && response.body));
            if (!response || !response.body || !response.clone) {
                window.__ds2api_error = 'invalid response';
                window.__ds2api_status = 'error';
                return response;
            }
            try {
                captureStream(response.clone().body);
            } catch(e) {
                ds2apiLog('capture err: ' + (e.message || e));
                window.__ds2api_error = e.message || String(e);
                window.__ds2api_status = 'error';
            }
            return response;
        }).catch(function(err){
            ds2apiLog('fetch err: ' + (err.message || err));
            window.__ds2api_error = err.message || String(err);
            window.__ds2api_status = 'error';
            throw err;
        });

        return promise;
    };
    window.__ds2api_fetch_hooked = true;

    var origXHROpen = window.XMLHttpRequest.prototype.open;
    window.XMLHttpRequest.prototype.open = function(method, url) {
        logUrl('xhr', url);
        var isCompletion = isCompletionUrl(url);
        var isChat = isChatUrl(url);
        ds2apiLog('XHR: ' + method + ' ' + (url || '').substring(0, 120) + ' completion=' + isCompletion + ' chat=' + isChat);

        if (isCompletion) {
            var xhr = this;
            var lastLen = 0;

            startStreamCapture();
            ds2apiLog('INTERCEPTING XHR completion: ' + url);

            var origSend = xhr.send;
            xhr.send = function(body) {
                try { xhr.overrideMimeType('text/plain; charset=utf-8'); } catch(e) {}
                return origSend.apply(this, arguments);
            };

            xhr.addEventListener('readystatechange', function() {
                if (xhr.readyState >= 3) {
                    try {
                        var respText = xhr.responseText || '';
                        var newText = respText.substring(lastLen);
                        lastLen = respText.length;

                        if (newText.length > 0) {
                            var lines = newText.split('\n');
                            for (var i = 0; i < lines.length; i++) {
                                processLine(lines[i]);
                            }
                        }
                    } catch(e) {
                        ds2apiLog('XHR readystatechange error: ' + (e.message || e));
                    }

                    if (xhr.readyState === 4) {
                        ds2apiLog('XHR done: status=' + xhr.status + ' totalLen=' + lastLen + ' chunks=' + window.__ds2api_chunks.length);
                        if (xhr.status !== 200) {
                            window.__ds2api_error = 'xhr status ' + xhr.status;
                            window.__ds2api_status = 'error';
                            return;
                        }
                        if (!window.__ds2api_done) {
                            window.__ds2api_done = true;
                            window.__ds2api_status = 'done';
                        }
                    }
                }
            });

            xhr.addEventListener('error', function() {
                ds2apiLog('XHR error');
                window.__ds2api_error = 'xhr error';
                window.__ds2api_status = 'error';
            });
        }

        return origXHROpen.apply(this, arguments);
    };
    window.__ds2api_xhr_hooked = true;

    var OrigEventSource = window.EventSource;
    window.EventSource = function(url, opts) {
        logUrl('es', url);
        var isCompletion = isCompletionUrl(url);
        ds2apiLog('EventSource: ' + (url || '').substring(0, 120) + ' completion=' + isCompletion);

        var es = new OrigEventSource(url, opts);

        if (isCompletion) {
            startStreamCapture();
            ds2apiLog('INTERCEPTING EventSource completion: ' + url);

            es.addEventListener('message', function(e) {
                ds2apiLog('ES msg: ' + (e.data || '').substring(0, 100));
                if (e.data && e.data !== '[DONE]') {
                    try {
                        JSON.parse(e.data);
                        window.__ds2api_chunks.push(e.data);
                    } catch(err) {}
                }
            });

            es.addEventListener('error', function(e) {
                ds2apiLog('ES error');
                window.__ds2api_error = 'eventsource error';
                window.__ds2api_status = 'error';
            });
        }

        return es;
    };
    window.EventSource.prototype = OrigEventSource.prototype;
    window.__ds2api_es_hooked = true;

    var OrigWebSocket = window.WebSocket;
    window.WebSocket = function(url, protocols) {
        logUrl('ws', url);
        ds2apiLog('WebSocket: ' + (url || '').substring(0, 120));
        var ws = protocols ? new OrigWebSocket(url, protocols) : new OrigWebSocket(url);

        ws.addEventListener('message', function(e) {
            var data = typeof e.data === 'string' ? e.data : '';
            if (data.indexOf('completion') !== -1 || data.indexOf('content') !== -1) {
                ds2apiLog('WS msg: ' + data.substring(0, 200));
                if (window.__ds2api_status !== 'streaming') {
                    startStreamCapture();
                }
                window.__ds2api_chunks.push(data);
            }
        });

        return ws;
    };
    WebSocket.prototype = OrigWebSocket.prototype;
    WebSocket.CONNECTING = OrigWebSocket.CONNECTING;
    WebSocket.OPEN = OrigWebSocket.OPEN;
    WebSocket.CLOSING = OrigWebSocket.CLOSING;
    WebSocket.CLOSED = OrigWebSocket.CLOSED;
    window.__ds2api_ws_hooked = true;

    if (typeof PerformanceObserver !== 'undefined') {
        try {
            var perfObs = new PerformanceObserver(function(list) {
                var entries = list.getEntries();
                for (var i = 0; i < entries.length; i++) {
                    var e = entries[i];
                    logUrl('perf', e.name);
                    if (e.name && (e.name.indexOf('completion') !== -1 || e.name.indexOf('chat') !== -1)) {
                        ds2apiLog('perf: ' + e.name.substring(0, 120) + ' type=' + e.entryType);
                    }
                }
            });
            perfObs.observe({entryTypes: ['resource']});
            window.__ds2api_perf_observer = true;
        } catch(e) {}
    }

    function captureStream(body){
        if (!body || typeof body.getReader !== 'function') {
            window.__ds2api_error = 'no readable body';
            window.__ds2api_status = 'error';
            return;
        }

        var reader = body.getReader();
        var decoder = new TextDecoder();
        var buffer = '';

        ds2apiLog('stream capture started');

        function read(){
            reader.read().then(function(result){
                if (result.done) {
                    flushBuffer(buffer);
                    buffer = '';
                    window.__ds2api_done = true;
                    window.__ds2api_status = 'done';
                    ds2apiLog('stream complete, chunks=' + window.__ds2api_chunks.length);
                    return;
                }

                var chunk = decoder.decode(result.value, {stream:true});
                buffer += chunk;

                var lines = buffer.split('\n');
                buffer = lines.pop();

                for(var i=0; i<lines.length; i++){
                    processLine(lines[i]);
                }

                read();
            }).catch(function(err){
                ds2apiLog('read error: ' + (err.message || err));
                window.__ds2api_error = err.message || String(err);
                window.__ds2api_status = 'error';
            });
        }

        read();
    }

    function processLine(line){
        line = line.trim();
        if(!line) return;

        if(line.startsWith('data:')){
            var dataStr = line.substring(5).trim();
            if(dataStr === '[DONE]'){
                ds2apiLog('processLine: got [DONE], chunks=' + window.__ds2api_chunks.length);
                window.__ds2api_done = true;
                window.__ds2api_status = 'done';
                return;
            }
            if(dataStr){
                try{
                    var parsed = JSON.parse(dataStr);
                    window.__ds2api_chunks.push(dataStr);
                    ds2apiLog('processLine: chunk added, total=' + window.__ds2api_chunks.length + ' preview=' + dataStr.substring(0, 60));
                } catch(e){
                    ds2apiLog('processLine parse warn: ' + dataStr.substring(0, 80));
                }
            }
        } else if(line.startsWith('event:')){
        } else if(line.startsWith(':')) {
        }
    }

    function processRawText(text){
        if (!text) return;
        var lines = text.split('\n');
        for (var i = 0; i < lines.length; i++) {
            processLine(lines[i]);
        }
    }

    function flushBuffer(buf){
        buf = buf.trim();
        if(!buf) return;
        var lines = buf.split('\n');
        for(var i=0; i<lines.length; i++){
            processLine(lines[i]);
        }
    }

    ds2apiLog('injection v2 complete - fetch/xhr/es/ws/perf hooked');
})();`
}

func ValidateSSEData(rawData string) ([]byte, bool) {
	rawData = strings.TrimSpace(rawData)
	if rawData == "" {
		return nil, false
	}
	if rawData == "[DONE]" {
		return []byte(rawData), true
	}
	return []byte(rawData), false
}
