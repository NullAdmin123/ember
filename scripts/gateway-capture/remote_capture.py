"""Linux-only finite AF_PACKET reader executed in an existing network namespace."""
import json
import signal
import socket
import struct
import sys
import time


def select_frame(frame, options):
    """Filter chosen IPv4 TCP ports and retain headers/handshakes with bounded sampling."""
    if len(frame) < 54 or frame[12:14] != b"\x08\x00" or frame[14] >> 4 != 4 or frame[23] != 6:
        return None
    ihl = (frame[14] & 15) * 4
    offset = 14 + ihl
    if ihl < 20 or len(frame) < offset + 20 or struct.unpack_from("!H", frame, 20)[0] & 0x3FFF:
        return None
    source, target = struct.unpack_from("!HH", frame, offset)
    http_ports = {options["gatewayPort"], options["embyPort"]}
    if not {source, target} & (http_ports | {options["tlsPort"]}):
        return None
    tcp_length = (frame[offset + 12] >> 4) * 4
    if tcp_length < 20 or len(frame) < offset + tcp_length:
        return None
    payload_offset = offset + tcp_length
    payload = frame[payload_offset:]
    snaplen = options["snaplen"]
    if target in http_ports:
        return frame[:snaplen]
    if source in http_ports and payload.startswith(b"HTTP/1."):
        end = payload.find(b"\r\n\r\n")
        return frame[:min(snaplen, payload_offset + end + 4)] if end >= 0 else frame[:snaplen]
    if payload.startswith(b"\x16\x03"):
        return frame[:snaplen]  # Public TLS handshakes only, no application decryption.
    return frame[:min(snaplen, 128)]


def record_fits(captured_bytes, stored_length, max_bytes):
    """Include the PCAP record header in the hard output capacity check."""
    return captured_bytes + 16 + stored_length <= max_bytes


def capture(options):
    """Capture in the foreground until time, byte limit or a stop signal; always close the socket."""
    reader = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(3))
    output = sys.stdout.buffer
    running = True

    def stop(signum, frame):
        """Stop this helper; never signal the Gateway application process."""
        nonlocal running
        running = False

    for name in ["SIGTERM", "SIGINT", "SIGHUP"]:
        signal.signal(getattr(signal, name), stop)
    packets, captured_bytes, reason = 0, 24, "duration_limit"
    try:
        reader.bind((options["interface"], 0))
        reader.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4194304)
        reader.settimeout(0.5)
        output.write(struct.pack("<IHHIIII", 0xA1B2C3D4, 2, 4, 0, 0, options["snaplen"], 1))
        output.flush()
        started, deadline = time.time(), time.monotonic() + options["durationSeconds"]
        print(json.dumps({"captureReady": True, "startedUtcEpoch": started, **options}), file=sys.stderr, flush=True)
        while running and time.monotonic() < deadline:
            try:
                frame = reader.recv(262144)
            except socket.timeout:
                continue
            stored = select_frame(frame, options)
            if stored is None:
                continue
            if not record_fits(captured_bytes, len(stored), options["maxBytes"]):
                reason = "byte_limit"
                break
            timestamp = time.time()
            seconds = int(timestamp)
            output.write(struct.pack("<IIII", seconds, int((timestamp - seconds) * 1e6), len(stored), len(frame)))
            output.write(stored)
            output.flush()
            packets += 1
            captured_bytes += len(stored) + 16
        if not running:
            reason = "signal"
    finally:
        try:
            received, dropped = struct.unpack("II", reader.getsockopt(263, 6, 8))
        except OSError:
            received = dropped = None
        reader.close()
        print(json.dumps({"captureFinished": True, "packets": packets, "capturedBytes": captured_bytes,
                          "endedUtcEpoch": time.time(), "stopReason": reason,
                          "socketPackets": received, "socketDroppedPackets": dropped}), file=sys.stderr, flush=True)


if __name__ == "__main__":
    capture(json.loads(sys.argv[1]))
