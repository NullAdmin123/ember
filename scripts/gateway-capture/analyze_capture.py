"""Extract bounded HTTP evidence without echoing credentials or arbitrary paths."""
from collections import Counter, defaultdict, deque
import hashlib
import hmac
import json
import re
import secrets
from urllib.parse import parse_qs, urlsplit

from wire import connection, records, tcp_packet

_REFERENCE_KEY = secrets.token_bytes(32)
_HTTP_START = re.compile(rb"(?:GET|HEAD|POST|PUT|DELETE|PATCH|OPTIONS) /|HTTP/1\.[01] [0-9]{3}")
_QUERY_KEYS = {"api_key", "x-emby-token", "playsessionid", "mediasourceid", "static",
               "container", "userid", "starttimeticks", "isplayback", "isstatic",
               "maxstreamingbitrate", "subtitleindex", "audiostreamindex"}


def reference(value):
    """Correlate within one analysis process using a fresh, unexported HMAC key."""
    return hmac.new(_REFERENCE_KEY, str(value).encode(), hashlib.sha256).hexdigest()[:16]


def safe_path(path):
    """Keep known playback routes; unknown paths may embed credentials and are omitted."""
    media = r"/(?:emby/)?(?:Videos|Items)/[0-9]{1,20}/(?:PlaybackInfo|stream(?:\.[a-z0-9]{1,12})?|original\.(?:mp4|mkv))"
    fixed = r"/(?:emby/)?(?:Sessions/Playing(?:/Progress|/Stopped)?|System/Info(?:/Public)?)"
    return path if re.fullmatch(media + "|" + fixed, path, re.I) else "[other]"


def http_summary(flow, timestamp, data, gateway_port=8081):
    """Expose fixed HTTP metadata and selected playback fields, never headers or bodies."""
    header, separator, body = data.partition(b"\r\n\r\n")
    if not separator:
        return None
    lines = header.decode("latin1").split("\r\n")
    headers = {}
    for line in lines[1:]:
        if ":" in line:
            key, value = line.split(":", 1)
            headers[key.lower()] = value.strip()
    event = {"epoch": round(timestamp, 6),
             "edge": "client_gateway" if gateway_port in (flow[1], flow[3]) else "gateway_emby",
             "connection": connection(flow)}
    if lines[0].startswith("HTTP/"):
        event.update(kind="response", status=int(lines[0].split(" ", 2)[1]))
        length = headers.get("content-length", "")
        if re.fullmatch(r"[0-9]{1,20}", length):
            event["content-length"] = length
        encoding = headers.get("content-encoding", "").lower()
        if encoding in {"identity", "gzip", "deflate", "br"}:
            event["content-encoding"] = encoding
        value = headers.get("content-range", "")
        if re.fullmatch(r"bytes (?:[0-9]+-[0-9]+|\*)/(?:[0-9]+|\*)", value):
            event["contentRange"] = value
        if "location" in headers:
            location = urlsplit(headers["location"])
            event["locationScheme"] = location.scheme if location.scheme in {"http", "https"} else "other"
            host = location.hostname or ""
            event["locationHost"] = host if re.fullmatch(r"[a-zA-Z0-9.-]{1,253}", host) else "unavailable"
        return event
    method, target, protocol = lines[0].split(" ", 2)
    if protocol not in {"HTTP/1.0", "HTTP/1.1"}:
        return None
    parsed = urlsplit(target)
    query = parse_qs(parsed.query, keep_blank_values=True, max_num_fields=256)
    event.update(kind="request", method=method, path=safe_path(parsed.path),
                 queryKeys=sorted({key.lower() if key.lower() in _QUERY_KEYS else "other" for key in query}))
    agent = re.search(r"\b(?:Infuse(?:-Direct)?|SenPlayer)/[0-9]{1,5}(?:\.[0-9]{1,5}){0,4}(?![0-9.])",
                      headers.get("user-agent", ""))
    event["client"] = agent.group(0) if agent else "other"
    value = headers.get("range", "")
    if re.fullmatch(r"bytes=(?:[0-9]+-[0-9]*|-[0-9]+)", value):
        event["range"] = value
    for key, values in query.items():
        if key.lower() == "playsessionid" and len(values) == 1:
            event["sessionRef"] = reference(values[0])
    authorization = headers.get("x-emby-authorization", headers.get("authorization", ""))
    device = re.search(r'DeviceId="([^"\r\n]{1,512})"', authorization, re.I)
    if device:
        event["deviceRef"] = reference(device.group(1))
    length = int(headers.get("content-length", "0"))
    event["bodyComplete"] = length >= 0 and len(body) >= length and "transfer-encoding" not in headers
    if 0 < length <= 65536 and event["bodyComplete"]:
        try:
            payload = json.loads(body[:length])
            if isinstance(payload, dict):
                for key in ["IsPlayback", "IsPaused"]:
                    if isinstance(payload.get(key), bool):
                        event[key] = payload[key]
                value = str(payload.get("ItemId", ""))
                if re.fullmatch(r"[0-9]{1,20}", value):
                    event["ItemId"] = value
                if isinstance(payload.get("MediaSourceId"), (str, int)):
                    event["mediaSourceRef"] = reference(payload["MediaSourceId"])
                value = payload.get("PositionTicks")
                if isinstance(value, int) and not isinstance(value, bool):
                    event["PositionTicks"] = value
                if isinstance(payload.get("PlaySessionId"), str):
                    event["sessionRef"] = reference(payload["PlaySessionId"])
        except (ValueError, UnicodeError, RecursionError):
            event["bodyDecoded"] = False
    return event


def analyze(path, gateway_port=8081, emby_port=18096, tls_port=443):
    """Reassemble consecutive TCP fragments only; gaps and capture truncation remain visible."""
    events, pending, seen, recent = [], {}, set(), deque()
    counts, flows = Counter(), Counter()
    first = last = None
    for timestamp, frame, original in records(path):
        first = timestamp if first is None else first
        last = timestamp
        counts["packets"] += 1
        packet = tcp_packet(frame, original)
        if packet is None:
            continue
        flow, data = packet["flow"], packet["payload"]
        if not {flow[1], flow[3]} & {gateway_port, emby_port, tls_port}:
            continue
        if len(flows) >= 4096 and connection(flow) not in flows:
            raise ValueError("too_many_capture_flows")
        flows[connection(flow)] += packet["declared"]
        if not data:
            continue
        identity = (flow, packet["sequence"], packet["declared"])
        if identity in seen:
            counts["duplicateSequencePackets"] += 1
            continue
        seen.add(identity)
        recent.append(identity)
        if len(recent) > 65536:
            seen.remove(recent.popleft())
        if tls_port in (flow[1], flow[3]):
            counts["tlsPayloadPackets"] += 1
            continue
        if _HTTP_START.match(data):
            pending[flow] = [timestamp, packet["sequence"] + len(data), data]
        elif flow in pending and packet["sequence"] == pending[flow][1]:
            pending[flow][1] += len(data)
            pending[flow][2] += data
        else:
            if flow in pending:
                counts["nonconsecutiveMessages"] += 1
                del pending[flow]
            continue
        entry = pending[flow]
        message = entry[2]
        split = message.find(b"\r\n\r\n")
        if len(message) > 98304:
            del pending[flow]
            counts["oversizedMessages"] += 1
            continue
        if split < 0:
            continue
        content_length = re.search(rb"\r\nContent-Length:\s*([0-9]+)", message[:split], re.I)
        body_length = int(content_length.group(1)) if content_length else 0
        if not message.startswith(b"HTTP/") and body_length <= 65536 and len(message) < split + 4 + body_length:
            continue
        try:
            event = http_summary(flow, entry[0], message, gateway_port)
            if event:
                events.append(event)
                if len(events) > 20000:
                    raise ValueError("too_many_http_events")
        except (ValueError, IndexError) as error:
            if str(error) == "too_many_http_events":
                raise
            counts["unparsedMessages"] += 1
        del pending[flow]
    counts["incompleteMessages"] = len(pending)
    return {"firstEpoch": first, "lastEpoch": last, "counts": dict(counts),
            "flowsPayloadBytes": dict(flows), "http": sorted(events, key=lambda event: event["epoch"])}


def video_pairs(events):
    """Pair observed HTTP/1 requests with final responses; do not infer missing requests."""
    queues, result = defaultdict(deque), []
    for event in events:
        if event["edge"] != "client_gateway":
            continue
        if event["kind"] == "request":
            queues[event["connection"]].append(event)
            continue
        if event["status"] < 200:
            continue
        source, target = event["connection"].split(">")
        queue = queues[f"{target}>{source}"]
        if not queue:
            continue
        request = queue.popleft()
        match = re.search(r"/Videos/([0-9]+)/", request["path"], re.I)
        if match:
            result.append({"itemId": match.group(1), "requestEpoch": request["epoch"],
                           "responseEpoch": event["epoch"], "status": event["status"],
                           "range": request.get("range"), "locationHost": event.get("locationHost"),
                           "packetHeadersMs": round((event["epoch"] - request["epoch"]) * 1000, 3)})
    return result
