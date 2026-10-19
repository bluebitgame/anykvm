"""Opt-in wire interoperability test using pyspnego's independent CredSSP client.

Uses disposable test credentials and stops before graphics/input activation.
"""

import socket
import select
import ssl
import struct
import sys

import spnego
from spnego.exceptions import SpnegoError


def recv_exact(sock, length):
    data = b""
    while len(data) < length:
        chunk = sock.recv(length - len(data))
        if not chunk:
            raise EOFError("server closed connection")
        data += chunk
    return data


def tpkt(data):
    return b"\x03\x00" + struct.pack(">H", len(data) + 4) + data


def ber(tag, data):
    length = len(data)
    size = bytes([length]) if length < 128 else b"\x82" + struct.pack(">H", length)
    return tag + size + data


def per_length(length):
    return bytes([length]) if length < 128 else struct.pack(">H", length | 0x8000)


def client_info():
    # SEC_INFO_PKT, Unicode, and five empty null-terminated credential strings.
    return struct.pack("<III5H", 0x40, 0, 0x10, 0, 0, 0, 0, 0) + b"\x00" * 10


def main():
    address, mode = sys.argv[1:]
    host, port = address.rsplit(":", 1)
    requested = 11 if mode == "hybrid-ex" else 3
    password = "wrong-password" if mode == "bad-password" else "test-password"
    kwargs = {}
    if mode != "spnego":
        kwargs["credssp_negotiate_context"] = spnego.client(
            "ANYKVM\\anykvm", password, protocol="ntlm"
        )
    context = spnego.client(
        "ANYKVM\\anykvm", password, protocol="credssp", credssp_min_protocol=5, **kwargs
    )
    with socket.create_connection((host, int(port)), timeout=10) as sock:
        request = b"\x0e\xe0\x00\x00\x00\x00\x00\x01\x00\x08\x00" + struct.pack("<I", requested)
        sock.sendall(tpkt(request))
        header = recv_exact(sock, 4)
        response = recv_exact(sock, struct.unpack(">H", header[2:])[0] - 4)
        assert response[7] == 2
        selected = struct.unpack("<I", response[11:])[0]
        assert selected == (8 if mode == "hybrid-ex" else 2)

        # Read complete TLS records, including when socket reads are fragmented.
        def tls_record():
            header = recv_exact(sock, 5)
            return header + recv_exact(sock, struct.unpack(">H", header[3:])[0])

        # pyspnego's token API expects a complete TLS handshake flight. Go
        # sends ServerHello and encrypted handshake messages in separate records.
        def tls_flight():
            records = [tls_record()]
            while select.select([sock], [], [], 0.05)[0]:
                try:
                    records.append(tls_record())
                except EOFError:
                    break  # Deliver a final CredSSP failure before socket EOF.
            return b"".join(records)

        incoming = None
        try:
            while not context.complete:
                outgoing = context.step(incoming)
                if outgoing:
                    sock.sendall(outgoing)
                if not context.complete:
                    # After TLS, keep each application record separate from
                    # close_notify so pyspnego can process a final error token.
                    incoming = tls_flight() if context.get_extra_info("auth_stage") == "TLS Handshake" else tls_record()
        except SpnegoError as error:
            status = getattr(error.base_error, "winerror", error.nt_status)
            if mode != "bad-password" or status != 0xC000006D:
                raise
            print("wrong password rejected with STATUS_LOGON_FAILURE")
            return
        assert mode != "bad-password", "wrong password was accepted"

        plaintext = b""

        def read_plain(length):
            nonlocal plaintext
            while len(plaintext) < length:
                try:
                    plaintext += context.unwrap(tls_record()).data
                except ssl.SSLWantReadError:
                    continue
            data, plaintext = plaintext[:length], plaintext[length:]
            return data

        def read_packet():
            header = read_plain(4)
            assert header[:2] == b"\x03\x00"
            return read_plain(struct.unpack(">H", header[2:])[0] - 4)

        def write_packet(data):
            sock.sendall(context.wrap(tpkt(data)).data)

        def write_share(data):
            write_packet(b"\x02\xf0\x80\x64\x00\x01\x03\xeb\x70" + per_length(len(data)) + data)

        def read_share(expected_type):
            packet = read_packet()
            assert packet[:4] == b"\x02\xf0\x80\x68"
            assert packet[6:8] == b"\x03\xeb"
            offset = 11 if packet[9] & 0x80 else 10
            data = packet[offset:]
            assert struct.unpack_from("<H", data)[0] == len(data)
            assert data[14] == expected_type, (data[14], expected_type)
            return data[18:]

        def share_control(kind, body):
            return struct.pack("<HHH", 6 + len(body), 0x10 | kind, 1002) + body

        def share_data(kind, body):
            header = struct.pack("<IBBHBBH", 0x10000, 0, 1, 12 + len(body), kind, 0, 0)
            return share_control(7, header + body)

        if mode == "hybrid-ex":
            assert read_plain(4) == b"\x00" * 4

        core = bytearray(142)
        struct.pack_into("<HH", core, 4, 640, 480)
        struct.pack_into("<H", core, 140, 0x100)
        channels = struct.pack("<I", 1) + b"drdynvc\x00" + b"\x00" * 4
        blocks = struct.pack("<HH", 0xC001, 4 + len(core)) + core
        blocks += struct.pack("<HH", 0xC003, 4 + len(channels)) + channels
        gcc = b"Duca" + per_length(len(blocks)) + blocks
        initial = ber(b"\x7f\x65", b"\x04\x00\x04\x00\x01\x00\x30\x00\x30\x00\x30\x00" + ber(b"\x04", gcc))
        write_packet(b"\x02\xf0\x80" + initial)
        assert read_packet()[:5] == b"\x02\xf0\x80\x7f\x66"
        write_packet(b"\x02\xf0\x80\x28")
        assert read_packet()[3] == 0x2E
        write_packet(b"\x02\xf0\x80\x38\x00\x01\x03\xeb")
        assert read_packet()[3] == 0x3E
        info = client_info()
        write_packet(b"\x02\xf0\x80\x64\x00\x01\x03\xeb\x70" + per_length(len(info)) + info)
        assert b"\x80\x00\x00\x00\xff\x03\x10\x00" in read_packet()
        demand = read_packet()
        assert demand[3] == 0x68

        # Advertise LONG_CREDENTIALS_SUPPORTED except in the raw case, which
        # independently checks the fixed-length legacy logon notification.
        long_credentials = mode != "raw"
        general = bytearray(20)
        struct.pack_into("<H", general, 10, 4 if long_credentials else 0)
        caps = struct.pack("<HH", 1, 24) + general
        confirm = struct.pack("<IHHHHH", 0x10000, 1001, 0, len(caps) + 4, 1, 0) + caps
        write_share(share_control(3, confirm))
        read_share(31)  # Synchronize
        read_share(20)  # Cooperate
        write_share(share_data(39, b"\x00" * 8))
        read_share(40)  # Font Map
        logon = read_share(38)
        info_type = struct.unpack_from("<I", logon)[0]
        info = logon[4:]
        if long_credentials:
            assert info_type == 1
            version, size, session_id, domain_len, user_len = struct.unpack_from("<HIIII", info)
            assert version == 1 and size == 576
            assert len(info) == size + domain_len + user_len
            domain = info[size:size + domain_len]
            username = info[size + domain_len:]
        else:
            assert info_type == 0 and len(info) == 576
            domain_len = struct.unpack_from("<I", info)[0]
            user_len = struct.unpack_from("<I", info, 56)[0]
            session_id = struct.unpack_from("<I", info, 572)[0]
            domain, username = info[4:4 + domain_len], info[60:60 + user_len]
        assert session_id != 0
        assert domain.decode("utf-16-le") == "ANYKVM\x00"
        assert username.decode("utf-16-le") == "anykvm\x00"
        print(f"{mode}: CredSSP, empty Client Info, and logon notification passed (session={session_id})")


if __name__ == "__main__":
    main()
