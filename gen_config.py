#!/usr/bin/env python3
"""Build a mihomo configuration from configured read-only source profiles.

All Japan nodes share a URL-test group, followed by the local fallback and
non-Japan backup groups. The candidate must pass mihomo validation before it
replaces the configuration file.
"""
import json
import hashlib
import os
import re
import secrets
import shutil
import sqlite3
import subprocess
import sys
import tempfile

import yaml

HOME = os.path.expanduser("~")
FLCLASH_DIR = os.path.join(HOME, "Library", "Application Support", "com.follow.clash")
SRHOP_DIR = os.path.join(HOME, "Library", "Application Support", "srhop")
MIHOMO_DIR = os.path.join(SRHOP_DIR, "mihomo")
CONFIG_PATH = os.path.join(MIHOMO_DIR, "config.yaml")
SRHOP_CONFIG = os.path.join(SRHOP_DIR, "config.json")

JP_RE = re.compile(r"(?i)(日本|JP|Japan|东京|大阪|名古屋|Tokyo|Osaka|Nagoya)")
INFO_RE = re.compile(r"剩余流量|套餐到期|距离下次|重置|官网|入口|流量|Expires")
GEO_RE = re.compile(r"^(GEOIP|GEOSITE|IP-ASN|RULE-SET),", re.I)


# Per-provider routing suffixes and a representative probe endpoint. Only the
# official domains of the providers selected in ai_targets are routed to the AI
# selector; shared identity providers and shared CDN parent domains stay out so
# other customers' traffic is not forced onto the AI route.
AI_PROVIDER_DEFS = {
    "openai": {
        "domains": ("openai.com", "chatgpt.com", "oaistatic.com",
                    "oaiusercontent.com", "openaiapi-site.azureedge.net"),
        "probe": "https://api.openai.com/v1/models",
        "expected": 401,
    },
    "claude": {
        "domains": ("anthropic.com", "claude.ai", "claude.com"),
        "probe": "https://api.anthropic.com/v1/models",
        "expected": 401,
    },
    "gemini": {
        "domains": ("generativelanguage.googleapis.com", "gemini.google.com",
                    "aistudio.google.com", "ai.google.dev"),
        "probe": "https://generativelanguage.googleapis.com/v1beta/models",
        "expected": 403,
    },
    "grok": {
        "domains": ("x.ai", "grok.com"),
        "probe": "https://api.x.ai/v1/models",
        "expected": 401,
    },
}
AI_EXPECTED_DEFAULT = 401


def ai_provider_domains(sc):
    """Routing suffixes for the selected providers, preserving selection order."""
    targets = sc.get("ai_targets")
    if not isinstance(targets, list) or not targets:
        targets = ["openai"]
    domains, seen = [], set()
    for target in targets:
        definition = AI_PROVIDER_DEFS.get(str(target).strip().lower())
        if not definition:
            continue
        for domain in definition["domains"]:
            if domain not in seen:
                seen.add(domain)
                domains.append(domain)
    return domains or list(AI_PROVIDER_DEFS["openai"]["domains"])


def log(msg):
    print(msg, file=sys.stderr, flush=True)


def sanitize(name):
    name = re.sub(r"[\U0001F000-\U0001FAFF\u2600-\u27BF]", "", str(name))
    name = re.sub(r"国内官网[：:].*$|固定入口[：:].*$", "", name)
    name = re.sub(r"[^A-Za-z0-9_.-]+", "-", name).strip("-")
    return name[:32] or "node"


def country(name, keywords=None):
    if (JP_RE.search(name) if keywords is None else any(k.casefold() in name.casefold() for k in keywords)):
        return "JP"
    for pat, cc in [
        (r"香港|HK|Hong", "HK"), (r"台湾|TW|Taiwan", "TW"),
        (r"新加坡|SG|Singapore", "SG"), (r"韩国|KR|Korea|首尔|Seoul", "KR"),
        (r"美国|USA|US|United", "US"), (r"马来西亚|MY|Malaysia", "MY"),
    ]:
        if re.search(pat, name, re.I):
            return cc
    return "XX"


def real_node(p):
    if not isinstance(p, dict):
        return False
    name = str(p.get("name", ""))
    if not name or not p.get("server") or not p.get("port"):
        return False
    if str(p.get("type", "")).lower() in ("subscribe", "direct", "reject"):
        return False
    return not INFO_RE.search(name)


def load(path):
    if not os.path.exists(path):
        return None
    with open(path, encoding="utf-8", errors="replace") as f:
        c = yaml.safe_load(f)
    return c if isinstance(c, dict) else None


def label_digest(profile, name):
    profile_bytes = profile.encode("utf-8")
    return hashlib.sha256(b"srhop-label-v1\0" + len(profile_bytes).to_bytes(8, "big") + profile_bytes + name.encode("utf-8")).hexdigest()


def take(conf, prefix, profile=None, keywords=None, include=(), exclude=()):
    if not conf:
        return [], []
    jp, other = [], []
    used = set()
    for i, p in enumerate(conf.get("proxies") or [], 1):
        if not (real_node(p) and node_matches_source(p, include, exclude)):
            continue
        cc = country(str(p.get("name", "")), keywords)
        label = sanitize(p.get("name", ""))
        label = re.sub(r"^JP[-_]*", "", label, flags=re.I)
        label = re.sub(r"-+", "-", label).strip("-")
        base = f"{prefix}-{cc}-{label or i}"
        name = base
        n = 2
        while name in used:
            name = f"{base}-{n}"
            n += 1
        used.add(name)
        if profile is not None:
            name += "-lbl-" + label_digest(profile, str(p["name"]))
        q = dict(p)
        q["name"] = name
        (jp if cc == "JP" else other).append(q)
    return jp, other


def existing_secret():
    if not os.path.exists(CONFIG_PATH):
        return None
    try:
        with open(CONFIG_PATH, encoding="utf-8", errors="replace") as f:
            for line in f:
                m = re.match(r"^secret:\s*(\S+)\s*$", line)
                if m:
                    return m.group(1).strip("\"'")
    except Exception:
        pass
    return None


def copy_geodata():
    os.makedirs(MIHOMO_DIR, exist_ok=True)
    copied = []
    for name in ("geoip.metadb", "GeoSite.dat", "GeoIP.dat", "ASN.mmdb"):
        src = os.path.join(FLCLASH_DIR, name)
        dst = os.path.join(MIHOMO_DIR, name)
        if os.path.exists(src):
            try:
                shutil.copy2(src, dst)
                copied.append(name)
            except Exception as e:
                log(f"warning: copy {name}: {e}")
    return copied


def srhop_settings():
    try:
        with open(SRHOP_CONFIG, encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return {}


def profile_label(path):
    # Only FlClash-owned profile IDs are eligible for database lookup.
    if os.path.dirname(os.path.abspath(path)) == os.path.join(FLCLASH_DIR, "profiles"):
        try:
            ident = int(os.path.splitext(os.path.basename(path))[0])
            db = sqlite3.connect("file:" + os.path.join(FLCLASH_DIR, "database.sqlite") + "?mode=ro", uri=True)
            try:
                row = db.execute("SELECT label FROM profiles WHERE id = ?", (ident,)).fetchone()
                if row and row[0]:
                    return row[0]
            finally:
                db.close()
        except (ValueError, sqlite3.Error):
            pass
    return os.path.basename(path)


def valid_source_entries(entries):
    return (isinstance(entries, list)
            and all(isinstance(e, dict) and isinstance(e.get("id"), str) and e["id"]
                    and isinstance(e.get("type"), str) for e in entries))


def node_matches_source(p, include, exclude):
    name = str(p.get("name", "")).casefold()
    return ((not include or any(k.casefold() in name for k in include))
            and not any(k.casefold() in name for k in exclude))


def legacy_source_entry(path):
    """Map a pre-sources profile path to a stable source entry."""
    base = os.path.splitext(os.path.basename(path))[0].lower()
    sid = re.sub(r"[^a-z0-9-]+", "-", base).strip("-") or "profile"
    label = profile_label(path)
    return {"id": sid, "label": label, "type": "profile", "path": path, "enabled": True}


def resolve_sources(sc):
    """Resolve enabled sources in priority order.

    Returns (active, meta): active are node-eligible entries (profile and
    subscription; subscription paths point at srhop's private node cache),
    meta covers every enabled source including vpn ones (labels metadata
    only, never node groups). SRHOP_SOURCE_PROFILES, when the daemon passes
    it, mirrors the derived source_profiles list and therefore gates which
    profile sources participate.
    """
    env_paths = None
    if "SRHOP_SOURCE_PROFILES" in os.environ:
        try:
            value = json.loads(os.environ["SRHOP_SOURCE_PROFILES"])
        except ValueError:
            raise ValueError("SRHOP_SOURCE_PROFILES must be a JSON list of paths")
        if not isinstance(value, list) or not all(isinstance(p, str) and p for p in value):
            raise ValueError("SRHOP_SOURCE_PROFILES must be a non-empty list of paths")
        env_paths = value

    if "sources" in sc:
        entries = [dict(e) for e in sc["sources"]]
        if not valid_source_entries(entries):
            raise ValueError("sources must be a non-empty list of {id,label,type} entries")
        ids = [e["id"] for e in entries]
        if len(set(ids)) != len(ids):
            raise ValueError("duplicate source id in sources")
        if env_paths is not None:
            allowed = set(env_paths)
            configured = {e.get("path") for e in entries if e.get("type") == "profile"}
            entries = [e for e in entries if e.get("type") != "profile" or e.get("path") in allowed]
            entries += [legacy_source_entry(p) for p in env_paths if p not in configured]
    else:
        paths = env_paths if env_paths is not None else sc.get("source_profiles", [])
        if not isinstance(paths, list) or not all(isinstance(p, str) and p for p in paths):
            raise ValueError("source_profiles must be a non-empty list of paths")
        entries, seen = [], set()
        for p in paths:
            entry = legacy_source_entry(p)
            n = 2
            while entry["id"] in seen:
                entry["id"] = "{}-{}".format(entry["id"], n)
                n += 1
            seen.add(entry["id"])
            entries.append(entry)

    # Discover additional local profiles without changing FlClash or config.json.
    # Existing configured paths, including disabled entries, remain authoritative.
    known_paths = {os.path.normpath(e.get("path", "")) for e in sc.get("sources", entries) if e.get("type") == "profile"}
    used_ids = {e["id"] for e in entries}
    db_path = os.path.join(FLCLASH_DIR, "database.sqlite")
    try:
        from pathlib import Path
        db = sqlite3.connect(Path(db_path).as_uri() + "?mode=ro", uri=True, timeout=0.2)
        try:
            rows = db.execute("SELECT id, url FROM profiles ORDER BY id LIMIT 10001").fetchall()
        finally:
            db.close()
        if len(rows) > 10000:
            raise ValueError("too many profiles")
        discovered = []
        for ident, url in rows:
            if type(ident) is not int or ident < 0:
                continue
            path = os.path.join(FLCLASH_DIR, "profiles", str(ident) + ".yaml")
            sid = "flclash-" + str(ident)
            if os.path.normpath(path) not in known_paths and sid not in used_ids:
                discovered.append({"id": sid, "label": "FlClash " + str(ident), "type": "profile", "path": path, "enabled": True, "discovered": True})
        entries.extend(discovered)
        urls = {str(i): u.strip() for i, u in rows if type(i) is int and i >= 0 and isinstance(u, str) and u.strip().startswith(("http://", "https://"))}
    except (sqlite3.Error, OSError, ValueError):
        urls = {}

    active, meta = [], []
    for e in entries:
        etype = e.get("type")
        if e.get("id") == "xlinker" or (e.get("id") == "office91" and sc.get("office91_enabled") is not True):
            continue
        if etype not in ("profile", "subscription", "vpn"):
            raise ValueError("source {}: unknown type {!r}".format(e.get("id"), etype))
        if not e.get("enabled", True):
            continue
        meta.append({"id": e["id"], "label": str(e.get("label") or e["id"])})
        if etype == "vpn":
            continue
        if etype == "profile":
            path = e.get("path")
            if not path:
                raise ValueError("source {}: profile source requires a path".format(e["id"]))
            profile_dir = os.path.join(FLCLASH_DIR, "profiles")
            ident = os.path.splitext(os.path.basename(path))[0]
            if os.path.dirname(os.path.normpath(path)) == profile_dir and re.fullmatch(r"[0-9]+", ident) and ident in urls:
                digest = hashlib.sha256(urls[ident].encode("utf-8")).hexdigest()
                cached = os.path.join(SRHOP_DIR, "sources", "flclash-" + ident + "-" + digest + ".yaml")
                if os.path.isfile(cached):
                    path = cached
        else:
            path = os.path.join(SRHOP_DIR, "sources", e["id"] + ".yaml")
        filters = {}
        for key in ("include_keywords", "exclude_keywords"):
            value = e.get(key) or []
            if isinstance(value, list) and not len(value) > 100:
                if any(not isinstance(k, str) or not k.strip() or len(k) > 256 for k in value):
                    raise ValueError("invalid source keyword filter")
            filters[key] = [k.strip() for k in value]
        active.append({"id": e["id"], "label": str(e.get("label") or e["id"]), "type": etype, "path": path, "discovered": e.get("discovered", False), **filters})
    return active, meta

def parse_fallback_address(value):
    if not isinstance(value, str):
        raise ValueError("fallback_addr must be host:port")
    host, sep, port = value.rpartition(":")
    if host.startswith("[") and host.endswith("]"):
        host = host[1:-1]
    elif ":" in host:
        raise ValueError("IPv6 fallback_addr must use [address]:port")
    if not sep or not host or any(c.isspace() for c in host) or not port.isascii() or not port.isdigit() or not 1 <= int(port) <= 65535:
        raise ValueError("fallback_addr must contain a host and port 1-65535")
    return host, int(port)


def build():
    sc = srhop_settings()
    keywords = sc.get("aggregate_keywords")
    if keywords is not None:
        if not isinstance(keywords, list) or not keywords or any(not isinstance(k, str) or not k.strip() for k in keywords):
            raise ValueError("aggregate_keywords must be a nonempty list of nonempty strings")
        keywords = [k.strip() for k in keywords]
    primary = sc.get("primary_addr", "127.0.0.1:33301")
    fallback = sc.get("fallback_addr", "127.0.0.1:33210")
    controller = sc.get("controller", "127.0.0.1:19090")
    probe = sc.get("probe_url", "https://www.gstatic.com/generate_204")
    ai_probe = sc.get("ai_probe_url", "https://api.openai.com/v1/models")
    try:
        engine_port = int(primary.rsplit(":", 1)[1])
    except Exception:
        engine_port = 33301
    fb_host, fb_port = parse_fallback_address(fallback)

    active, source_meta = resolve_sources(sc)
    base_id = sc.get("base_rules_source")
    if not isinstance(base_id, str) or not base_id:
        base_id = active[0]["id"] if active else ""

    loaded = []
    for e in active:
        conf = load(e["path"])
        if conf is None:
            if e["type"] == "subscription" or e.get("discovered"):
                # 玖壹 node cache missing: the refresher owns the error state.
                # Generation continues with the remaining sources instead of
                # faking this one or failing everything else.
                continue
            raise ValueError("source {} could not be read".format(e["id"]))
        loaded.append((e, conf))
    base_conf = next((conf for e, conf in loaded if e["id"] == base_id), None)
    if base_conf is None:
        # A missing subscription cache is a transient state: keep generating with
        # the remaining sources and let the refresher report the failure.
        if base_id and not any(e["id"] == base_id and e["type"] == "subscription" for e in active):
            raise ValueError("base_rules_source {!r} is missing, disabled, or unreadable".format(base_id))
        base_conf = {}

    sources_jp = []
    other = []
    labels = {}
    for e, conf in loaded:
        include, exclude = e["include_keywords"], e["exclude_keywords"]
        jp, rest = take(conf, e["id"], e["label"], keywords, include, exclude)
        sources_jp.append((e, jp + rest))
        other.extend(rest)
        originals = [p for p in conf.get("proxies", []) if real_node(p) and node_matches_source(p, include, exclude)]
        # take preserves source order within each country partition.
        original_jp = [p for p in originals if country(str(p["name"]), keywords) == "JP"]
        original_other = [p for p in originals if country(str(p["name"]), keywords) != "JP"]
        for generated, original in zip(jp + rest, original_jp + original_other):
            labels[generated["name"]] = {"profile": e["label"], "name": str(original["name"]), "source_id": e["id"]}
    all_jp = [p for _, jp in sources_jp for p in jp]
    proxies = list(all_jp)
    proxies.append({"name": "FALLBACK-33210", "type": "http",
                    "server": fb_host, "port": fb_port})

    def url_test(name, members, url, status, interval):
        return {
            "name": name,
            "type": "url-test",
            "proxies": [p["name"] for p in members],
            "url": url,
            "expected-status": status,
            "interval": interval,
            "tolerance": 50,
            "timeout": 5000,
            "lazy": False,
        }

    groups = []
    chain = []
    for e, jp in sources_jp:
        if not jp:
            continue
        groups.append(url_test("JP-{}".format(e["id"]), jp, probe, 204, 60))
        chain.append("JP-{}".format(e["id"]))
    if all_jp:
        groups.append(url_test("JP-ALL", all_jp, probe, 204, 60))
    if other:
        groups.append(url_test("BACKUP-AUTO", other, probe, 204, 120))
    ai_chain = []
    for e, jp in sources_jp:
        if not jp:
            continue
        groups.append(url_test("AI-JP-{}".format(e["id"]), jp, ai_probe, 401, 60))
        ai_chain.append("AI-JP-{}".format(e["id"]))
    if all_jp:
        groups.append(url_test("AI-JP-ALL", all_jp, ai_probe, 401, 60))
    for name, members, target, status in (
        ("BOOT-JP-ALL", chain, probe, 204),
        ("BOOT-AI-JP-ALL", ai_chain, ai_probe, 401),
    ):
        # The startup chain must not silently select the external fallback.
        # Keep it available as an explicit selector member for the runtime's
        # failure policy, and do not emit empty fallback groups.
        if members:
            groups.append({"name": name, "type": "fallback", "proxies": members,
                           "url": target, "expected-status": status,
                           "interval": 20, "timeout": 5000, "lazy": False})
    groups.append({"name": "主代理", "type": "select",
                   "proxies": ((["BOOT-JP-ALL"] if chain else []) + ["FALLBACK-33210"]
                               + [p["name"] for p in all_jp])})
    groups.append({"name": "AI主代理", "type": "select",
                   "proxies": ((["BOOT-AI-JP-ALL"] if ai_chain else []) + ["FALLBACK-33210"]
                               + [p["name"] for p in all_jp])})

    base_rules = list(base_conf.get("rules") or [])
    rules = []
    for r in base_rules:
        s = str(r).strip()
        if s.upper().startswith("MATCH,"):
            continue
        s = s.replace(",大机场 Big Airport", ",主代理")
        s = s.replace(",自动选择", ",主代理")
        s = s.replace(",故障转移", ",主代理")
        rules.append(s)
    ai_rules = [f"DOMAIN-SUFFIX,{domain},AI主代理" for domain in ai_provider_domains(sc)]
    rules = ai_rules + rules + ["MATCH,主代理"]

    cfg = {
        "mixed-port": engine_port,
        "allow-lan": False,
        "bind-address": "127.0.0.1",
        "mode": "rule",
        "log-level": "info",
        "ipv6": False,
        "unified-delay": True,
        "tcp-concurrent": True,
        "external-controller": controller,
        "secret": existing_secret() or secrets.token_hex(16),
        "profile": {"store-selected": True, "store-fake-ip": False},
        "tun": {"enable": False},
        "dns": {
            "enable": True,
            "ipv6": False,
            "enhanced-mode": "redir-host",
            "nameserver": ["223.5.5.5", "119.29.29.29"],
        },
        "proxies": proxies,
        "proxy-groups": groups,
        "rules": rules,
    }

    # Do not mutate live geodata during candidate generation.
    if not os.path.exists(os.path.join(MIHOMO_DIR, "geoip.metadb")):
        cfg["rules"] = [r for r in cfg["rules"] if not GEO_RE.search(r)]

    os.makedirs(MIHOMO_DIR, exist_ok=True)
    mihomo = sc.get("engine_bin", "/opt/homebrew/bin/mihomo")
    fd, tmp = tempfile.mkstemp(prefix=".config-", suffix=".yaml", dir=MIHOMO_DIR)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            yaml.safe_dump(cfg, f, allow_unicode=True, sort_keys=False, width=10000)
        r = subprocess.run([mihomo, "-d", MIHOMO_DIR, "-t", "-f", tmp], capture_output=True, text=True)
        if r.returncode != 0:
            log((r.stdout + r.stderr)[-3000:])
            raise ValueError("mihomo rejected the generated config")
        # Stage both files before publishing; roll back the config if the mapping
        # cannot be published. Cross-file visibility cannot be atomic.
        label_path = os.path.join(SRHOP_DIR, "node-labels.json")
        os.makedirs(SRHOP_DIR, exist_ok=True)
        label_fd, label_tmp = tempfile.mkstemp(prefix=".node-labels-", dir=SRHOP_DIR)
        try:
            with os.fdopen(label_fd, "w", encoding="utf-8") as f:
                with open(tmp, "rb") as config_file:
                    config_hash = hashlib.sha256(config_file.read()).hexdigest()
                # Reserved top-level keys (underscore prefix, never node
                # names): generation id of the emitted config.yaml and the
                # id→label map of every enabled source of this generation.
                json.dump({"config_sha256": config_hash,
                           "_generation": config_hash[:16],
                           "_sources": {m["id"]: m["label"] for m in source_meta},
                           "nodes": labels}, f, ensure_ascii=False)
            old = None
            if os.path.exists(CONFIG_PATH):
                with open(CONFIG_PATH, "rb") as f:
                    old = f.read()
            os.replace(tmp, CONFIG_PATH)
            try:
                os.replace(label_tmp, label_path)
            except Exception:
                if old is None:
                    os.unlink(CONFIG_PATH)
                else:
                    fd2, rollback = tempfile.mkstemp(prefix=".rollback-", dir=MIHOMO_DIR)
                    with os.fdopen(fd2, "wb") as f:
                        f.write(old)
                    os.replace(rollback, CONFIG_PATH)
                raise
        finally:
            if os.path.exists(label_tmp):
                os.unlink(label_tmp)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)

    def mtime(path):
        return int(os.path.getmtime(path)) if os.path.exists(path) else 0

    return {
        "config": CONFIG_PATH,
        "jp": len(all_jp),
        "other": len(other),
        "groups": [g["name"] for g in groups],
        "rules": len(rules),
        "sources": [{"id": e["id"], "path": e["path"], "mtime": mtime(e["path"])} for e, _ in loaded],
    }


if __name__ == "__main__":
    print(json.dumps(build(), ensure_ascii=False))
