Vendored unchanged from the official Moby profiles repository:
https://github.com/moby/profiles/blob/4b8214fc4971e06172938a1f1554ccff6647ee80/seccomp/default.json

Commit: 4b8214fc4971e06172938a1f1554ccff6647ee80
License: Apache-2.0; adjacent LICENSE is copied from that commit.

Runtime transformations only remove permissions: socketcall/socketpair/io_uring_setup are removed; network deny removes socket entirely; network inherit splits the original AF_UNSPEC/AF_UNIX/AF_INET rule to preserve AF_UNSPEC and AF_INET while omitting AF_UNIX. Other socket-family rules and all baseline syscall restrictions remain unchanged. The client reads the generated temporary profile; it need not be shared with a remote daemon.

SHA-256 of unchanged vendored default.json: 6416b47770785a41ac59073cdc77d9fe98517df2799dc83ef207e622de3053f6
