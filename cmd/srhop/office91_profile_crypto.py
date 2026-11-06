"""Frozen offline importer primitives; contains no account key material."""
import os
import stat
import yaml
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
KNOWN = {'ss', 'ssr', 'vmess', 'vless', 'trojan', 'http', 'socks5', 'hysteria', 'hysteria2', 'hy2', 'tuic', 'wireguard', 'anytls', 'ssh', 'snell', 'mieru'}
class UniqueLoader(yaml.SafeLoader):
    pass

def construct_mapping(loader, node, deep=False):
    loader.flatten_mapping(node)
    result = {}
    for knode, vnode in node.value:
        key = loader.construct_object(knode, deep=deep)
        try:
            if key in result:
                raise ValueError('duplicate mapping key')
            result[key] = loader.construct_object(vnode, deep=deep)
        except TypeError:
            raise ValueError('invalid mapping key') from None
    return result

UniqueLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, construct_mapping)

def derive():
    # This is an already-derived AES key, kept out of source and process args.
    path = os.environ.get("SRHOP_OFFLINE_KEY_FILE", "")
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, "rb") as source:
        st = os.fstat(source.fileno())
        if not stat.S_ISREG(st.st_mode) or st.st_uid != os.getuid() or stat.S_IMODE(st.st_mode) != 0o600:
            raise ValueError("private key required")
        key = source.read(33)
    if len(key) != 32:
        raise ValueError("invalid key material")
    return key
