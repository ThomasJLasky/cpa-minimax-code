#!/usr/bin/env python3
"""把动态库打成 CLIProxyAPI 插件商店要求的 zip，并输出 sha256。

商店约定：
    <id>_<version>_<goos>_<goarch>.zip   内含 <id>.dylib / .so / .dll（根目录）
    checksums.txt                        sha256sum 格式
"""
import argparse
import hashlib
import os
import zipfile


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--version", required=True)
    parser.add_argument("--goos", required=True)
    parser.add_argument("--goarch", required=True)
    parser.add_argument("--lib", required=True)
    parser.add_argument("--id", default="minimax-code")
    parser.add_argument("--outdir", default="dist")
    args = parser.parse_args()

    os.makedirs(args.outdir, exist_ok=True)
    name = f"{args.id}_{args.version}_{args.goos}_{args.goarch}.zip"
    path = os.path.join(args.outdir, name)

    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as zf:
        zf.write(args.lib, arcname=os.path.basename(args.lib))

    digest = hashlib.sha256(open(path, "rb").read()).hexdigest()
    print(f"{name}\nsha256 {digest}")

    sums = os.path.join(args.outdir, "checksums.txt")
    line = f"{digest}  {name}\n"
    with open(sums, "a", encoding="utf-8") as fh:
        fh.write(line)


if __name__ == "__main__":
    main()
