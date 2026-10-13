"""Extract plaintext TLS ClientHello hostnames and TCP handshake timing only."""
from collections import Counter
import re
import struct
from wire import connection, records, tcp_packet

def client_hello_hostname(payload):
    """Read a complete SNI extension from a captured ClientHello prefix."""
    try:
        if len(payload) < 44 or payload[:2] != b"\x16\x03" or payload[5] != 1:
            return None
        cursor = 9 + 2 + 32
        cursor += 1 + payload[cursor]
        cipher_length = struct.unpack_from("!H", payload, cursor)[0]
        cursor += 2 + cipher_length
        cursor += 1 + payload[cursor]
        extension_length = struct.unpack_from("!H", payload, cursor)[0]
        cursor += 2
        end = min(len(payload), cursor + extension_length)
        while cursor + 4 <= end:
            kind, length = struct.unpack_from("!HH", payload, cursor)
            cursor += 4
            if kind == 0 and cursor + length <= len(payload):
                name_list_length = struct.unpack_from("!H", payload, cursor)[0]
                position = cursor + 2
                name_end = min(cursor + length, position + name_list_length)
                while position + 3 <= name_end:
                    name_type, name_length = struct.unpack_from("!BH", payload, position)
                    position += 3
                    if name_type == 0 and position + name_length <= name_end:
                        name = payload[position:position + name_length].decode("ascii").lower()
                        return name if re.fullmatch(r"[a-z0-9.-]{1,253}", name) else None
                    position += name_length
            cursor += length
    except (IndexError, struct.error, UnicodeError):
        return None
    return None

def analyze_tls(path, tls_port=443):
    """Summarize captured IPv4 TLS flows without decrypting application data."""
    flows = {}
    for now, frame, original in records(path):
        packet = tcp_packet(frame, original)
        if packet is None:
            continue
        source, source_port, destination, destination_port = packet["flow"]
        if tls_port not in (source_port, destination_port):
            continue
        outbound = destination_port == tls_port
        flow_key = packet["flow"] if outbound else (destination, destination_port, source, source_port)
        key = connection(flow_key)
        sequence = packet["sequence"]
        if len(flows) >= 4096 and key not in flows:
            raise ValueError("too_many_tls_flows")
        flow = flows.setdefault(key, {"connection":key, "hostname":None,
            "firstEpoch":now, "lastEpoch":now, "synSequences":Counter(),
            "firstSynEpoch":None, "firstSynAckEpoch":None,
            "firstClientHelloEpoch":None, "firstServerHelloEpoch":None,
            "outboundApplicationRecords":0, "inboundApplicationRecords":0})
        flow["lastEpoch"] = now
        flags = packet["flags"]
        if outbound and flags & 2 and not flags & 16:
            flow["synSequences"][sequence] += 1
            if flow["firstSynEpoch"] is None:
                flow["firstSynEpoch"] = now
        elif not outbound and flags & 2 and flags & 16 and flow["firstSynAckEpoch"] is None:
            flow["firstSynAckEpoch"] = now
        payload = packet["payload"]
        if len(payload) < 6 or payload[1] != 3:
            continue
        if payload[0] == 22 and payload[5] == 1 and outbound:
            flow["hostname"] = client_hello_hostname(payload) or flow["hostname"]
            if flow["firstClientHelloEpoch"] is None:
                flow["firstClientHelloEpoch"] = now
        elif payload[0] == 22 and payload[5] == 2 and not outbound:
            if flow["firstServerHelloEpoch"] is None:
                flow["firstServerHelloEpoch"] = now
        elif payload[0] == 23:
            flow["outboundApplicationRecords" if outbound else "inboundApplicationRecords"] += 1
    result = []
    for flow in flows.values():
        sequences = flow.pop("synSequences")
        flow["synRetransmissions"] = sum(max(0, count - 1) for count in sequences.values())
        flow["synCount"] = sum(sequences.values())
        flow["newConnectionObserved"] = flow["firstSynEpoch"] is not None
        if flow["firstSynEpoch"] is not None and flow["firstSynAckEpoch"] is not None:
            flow["tcpConnectMs"] = round((flow["firstSynAckEpoch"] - flow["firstSynEpoch"]) * 1000, 3)
        result.append(flow)
    return result
