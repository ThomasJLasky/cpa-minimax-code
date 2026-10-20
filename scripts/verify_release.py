#!/usr/bin/env python3
"""核对 GitHub Release 里已发布的 zip，不用把整个包下载下来。

只发两次 Range 请求（取 EOCD + 中央目录），据此判断：
  1) zip 里是否恰好一个文件，且名字是 minimax-code.<ext>
  2) 该文件不是空文件
  3) 各平台 zip 的扩展名正确

用法：
    python3 scripts/verify_release.py <owner> <repo> <tag>

对 darwin_arm64 / linux 这类本机可执行的产物，可以再用测试桩加载验证：
    curl -sSL -o m.zip <release 下载地址>
    python3 -c "import zipfile;zipfile.ZipFile('m.zip').extractall('out')"
    go build -o /tmp/plugtest ./testharness && /tmp/plugtest out/minimax-code.so register
"""
import json
import struct
import sys
import urllib.request
import zipfile

CHUNK = 65536


def http_get(url: str, headers: dict[str, str] | None = None) -> tuple[bytes, dict[str, str]]:
    req = urllib.request.Request(url, headers={"Accept": "application/octet-stream", **(headers or {})})
    with urllib.request.urlopen(req, timeout=60) as resp:
        return resp.read(), dict(resp.headers)


def release_assets(owner: str, repo: str, tag: str) -> dict[str, str]:
    body, _ = http_get(f"https://api.github.com/repos/{owner}/{repo}/releases/tags/{tag}", {"Accept": "application/vnd.github+json"})
    data = json.loads(body)
    return {asset["name"]: asset["url"] for asset in data.get("assets", [])}


def inspect_zip(url: str) -> tuple[int, list[zipfile.ZipInfo]]:
    _, headers = http_get(url, {"Range": "bytes=0-0"})
    total = int(headers["Content-Range"].split("/")[-1])

    # EOCD 在文件末尾最多 22 + 最大注释长度，取最后 64KB 足够。
    start = max(0, total - CHUNK)
    tail, _ = http_get(url, {"Range": f"bytes={start}-{total - 1}"})
    idx = tail.rfind(b"PK\x05\x06")
    if idx < 0:
        raise RuntimeError("end of central directory not found")
    eocd = tail[idx : idx + 22]
    if len(eocd) < 22:
        raise RuntimeError("incomplete EOCD signature")
    _, _, _, _, cd_size, cd_offset = struct.unpack("<IHHHHII", eocd)

    cd, _ = http_get(url, {"Range": f"bytes={cd_offset}-{cd_offset + cd_size - 1}"})
    infos: list[zipfile.ZipInfo] = []
    pos = 0
    while pos + 46 <= len(cd) and cd[pos : pos + 4] == b"PK\x01\x02":
        (
            _sig, _made_by, _need, _flags, _method, _mtime, _mdate,
            _crc, csize, usize, nlen, elen, clen,
            _disk, _iattr, _eattr, _lh,
        ) = struct.unpack("<IHHHHHHIIIHHHHHII", cd[pos : pos + 46])
        name = cd[pos + 46 : pos + 46 + nlen].decode("utf-8", "replace")
        info = zipfile.ZipInfo(name)
        info.file_size = usize
        info.compress_size = csize
        infos.append(info)
        pos += 46 + nlen + elen + clen
    if not infos:
        raise RuntimeError("central directory parsed no entries")
    return total, infos


def main() -> int:
    if len(sys.argv) != 4:
        print(__doc__)
        return 2
    owner, repo, tag = sys.argv[1:4]
    try:
        assets = release_assets(owner, repo, tag)
    except Exception as exc:  # noqa: BLE001
        print(f"读取 release 失败: {exc}")
        return 1

    zips = sorted(name for name in assets if name.endswith(".zip"))
    if not zips:
        print("release 里没有 zip 资产")
        return 1

    failed = False
    for name in zips:
        ext = "dylib" if "darwin" in name else "dll" if "windows" in name else "so"
        want = f"minimax-code.{ext}"
        try:
            total, infos = inspect_zip(assets[name])
        except Exception as exc:  # noqa: BLE001
            print(f"FAIL {name}: {exc}")
            failed = True
            continue
        names = [(info.filename, info.file_size) for info in infos]
        ok = names == [(want, names[0][1])] and names[0][1] > 0
        if not ok:
            failed = True
        print(f"{'ok  ' if ok else 'FAIL'} {name}  {total / 1048576:.2f} MiB  ->  {names}")

    print("\n结论:", "PASS 布局全部正确" if not failed else "FAIL 有资产不符合约定")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
