"""Read classic Ethernet PCAP records with bounded lengths and IPv4 TCP checks."""
import socket
import struct


def records(path):
    """Yield complete records; reject unsupported formats and truncated artifacts."""
    formats = {b"\xd4\xc3\xb2\xa1": ("<", 1e6), b"\xa1\xb2\xc3\xd4": (">", 1e6),
               b"\x4d\x3c\xb2\xa1": ("<", 1e9), b"\xa1\xb2\x3c\x4d": (">", 1e9)}
    with open(path, "rb") as stream:
        header = stream.read(24)
        if len(header) != 24 or header[:4] not in formats:
            raise ValueError("unsupported_pcap")
        endian, scale = formats[header[:4]]
        major, minor, _, _, snaplen, linktype = struct.unpack(endian + "HHIIII", header[4:])
        if (major, minor) != (2, 4) or linktype != 1 or not 54 <= snaplen <= 262144:
            raise ValueError("unsupported_pcap_linktype_or_snaplen")
        while True:
            record = stream.read(16)
            if not record:
                return
            if len(record) != 16:
                raise ValueError("truncated_pcap_record")
            seconds, fraction, included, original = struct.unpack(endian + "IIII", record)
            if included > snaplen or included > original or fraction >= scale:
                raise ValueError("invalid_pcap_record_length_or_timestamp")
            frame = stream.read(included)
            if len(frame) != included:
                raise ValueError("truncated_pcap_frame")
            yield seconds + fraction / scale, frame, original


def tcp_packet(frame, original):
    """Decode a complete IPv4 TCP header; skip fragments, IPv6 and malformed frames."""
    if len(frame) < 54 or frame[12:14] != b"\x08\x00" or frame[14] >> 4 != 4 or frame[23] != 6:
        return None
    ihl = (frame[14] & 15) * 4
    offset = 14 + ihl
    if ihl < 20 or len(frame) < offset + 20 or struct.unpack_from("!H", frame, 20)[0] & 0x3FFF:
        return None
    tcp_length = (frame[offset + 12] >> 4) * 4
    ip_length = struct.unpack_from("!H", frame, 16)[0]
    # GRO/GSO captures may leave the IPv4 total-length field at zero.
    end = 14 + ip_length if ip_length else original
    payload_offset = offset + tcp_length
    if tcp_length < 20 or len(frame) < payload_offset or end < payload_offset:
        return None
    sport, dport, sequence = struct.unpack_from("!HHI", frame, offset)
    source, target = socket.inet_ntoa(frame[26:30]), socket.inet_ntoa(frame[30:34])
    return {"flow": (source, sport, target, dport), "sequence": sequence,
            "flags": frame[offset + 13], "declared": end - payload_offset,
            "payload": frame[payload_offset:min(len(frame), end)]}


def connection(flow):
    """Render network endpoints without ever including application credentials."""
    return f"{flow[0]}:{flow[1]}>{flow[2]}:{flow[3]}"
