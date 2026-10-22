#!/usr/bin/env python3

# Report last Hydra build status for given platforms and packages. All supported
# platforms will be checked, if no platform is specified. Fail with exit code 1
# if status of any checked package is not good.
#
# Optimized with binary cache pre-checks:
# If a package's output store path is known, checks `cache.nixos.org` first via
# concurrent HTTP HEAD requests. Only packages that miss the cache (or lack a store path)
# fall back to querying Hydra.

# USAGE:
# hydra-build-status.py --file <PACKAGES-FILE.json> --platforms=[PLATFORM,PLATFORM,...] [--failed-only]

import concurrent.futures
import datetime
from getopt import getopt
import json
import os
import sys
import time
from bs4 import BeautifulSoup
import requests
from requests.adapters import HTTPAdapter

USER_AGENT = "hydra-build-status.py; Nix Geospatial team; Ivan Mincik (@imincik)"
HYDRA_URL = "https://hydra.nixos.org"
CACHE_URL = "https://cache.nixos.org"
OK_STATUSES = [
    "Succeeded",
    "Succeeded (cached)",
    "Cancelled",
    "No data",
    "No recent data",
]

opts, args = getopt(
    sys.argv[1:],
    "f:p:x",
    [
        "file=",
        "platforms=",
        "failed-only",
        "project=",
        "jobset=",
        "cache-url=",
        "no-cache",
        "cache-only",
        "workers=",
    ],
)

# list of platforms
platforms = ["x86_64-linux", "aarch64-linux", "x86_64-darwin", "aarch64-darwin"]
failed_only = False
project = "nixpkgs"
jobset = "unstable"
cache_url = CACHE_URL
use_cache = True
cache_only = False
workers = 20
pkgs_file = None

for opt, arg in opts:
    if opt in ["-p", "--platforms"]:
        platforms = arg.split(",")
    elif opt in ["-f", "--file"]:
        pkgs_file = arg
    elif opt in ["-x", "--failed-only"]:
        failed_only = True
    elif opt == "--project":
        project = arg
    elif opt == "--jobset":
        jobset = arg
    elif opt == "--cache-url":
        cache_url = arg
    elif opt == "--no-cache":
        use_cache = False
    elif opt == "--cache-only":
        cache_only = True
    elif opt == "--workers":
        workers = int(arg)

if not pkgs_file:
    sys.stderr.write("Error: --file argument is required.\n")
    sys.exit(2)


def normalize_packages(raw_pkgs):
    """Normalize various input formats (list of strings, list of dicts, or dict of packages)
    into a list of dicts: [{'name': ..., 'path': ..., 'system': ...}]
    """
    if (
        isinstance(raw_pkgs, dict)
        and "packages" in raw_pkgs
        and isinstance(raw_pkgs["packages"], list)
    ):
        raw_pkgs = raw_pkgs["packages"]

    pkgs = []
    if isinstance(raw_pkgs, list):
        for item in raw_pkgs:
            if isinstance(item, dict):
                name = item.get("name") or item.get("pname") or item.get("attr")
                path = (
                    item.get("path")
                    or item.get("storePath")
                    or item.get("out")
                    or item.get("outPath")
                )
                sys_name = item.get("system") or item.get("platform")
                if name:
                    pkgs.append({"name": name, "path": path, "system": sys_name})
            elif isinstance(item, str):
                pkgs.append({"name": item, "path": None, "system": None})
    elif isinstance(raw_pkgs, dict):
        for name, val in raw_pkgs.items():
            if isinstance(val, str):
                pkgs.append({"name": name, "path": val, "system": None})
            elif isinstance(val, dict):
                path = (
                    val.get("path")
                    or val.get("storePath")
                    or val.get("out")
                    or val.get("outPath")
                )
                sys_name = val.get("system") or val.get("platform")
                pkgs.append({"name": name, "path": path, "system": sys_name})
            else:
                pkgs.append({"name": name, "path": None, "system": None})
    return pkgs


with open(pkgs_file, "r") as file:
    raw_pkgs = json.load(file)

pkgs = normalize_packages(raw_pkgs)


def print_header(platform):
    print(f"\n### PLATFORM: {platform}\n")
    print(
        "| {f1: <50} | {f2: <20} | {f3: <80} |".format(
            f1="PACKAGE", f2="STATUS", f3="URL"
        )
    )
    print("| {sep: <50} | {sep: <20} | {sep: <80} |".format(sep=20 * "-"))


session = requests.Session()
adapter = HTTPAdapter(pool_connections=workers, pool_maxsize=workers, max_retries=2)
session.mount("https://", adapter)
session.mount("http://", adapter)


def check_cache_for_pkg(item):
    name = item["name"]
    path = item.get("path")
    if not path or not isinstance(path, str):
        return name, None, None

    base = os.path.basename(path)
    store_hash = base.split("-", 1)[0]
    if len(store_hash) != 32:
        return name, None, None

    url = f"{cache_url.rstrip('/')}/{store_hash}.narinfo"
    try:
        res = session.head(url, headers={"User-Agent": USER_AGENT}, timeout=10)
        if res.status_code == 200:
            return name, True, url
        elif res.status_code == 404:
            return name, False, url
        else:
            return name, None, url
    except Exception:
        return name, None, url


def query_hydra_status(pkg_name, platform):
    headers = {"User-Agent": USER_AGENT}
    url = f"{HYDRA_URL}/job/{project}/{jobset}/{pkg_name}.{platform}/all"
    try:
        page = session.get(url, headers=headers, timeout=20)
        soup = BeautifulSoup(page.content, "html.parser")
        results_table = soup.find("table", class_="table")

        if results_table:
            build_results = results_table.find_all("tr")
            if len(build_results) > 1:
                build_result = build_results[1].find_all("td")
                now_time = datetime.datetime.now()
                build_time = datetime.datetime.fromtimestamp(
                    int(build_result[2].time["data-timestamp"])
                )
                if (now_time - build_time).days < 365:
                    build_status = build_result[0].img["alt"]
                else:
                    build_status = "No recent data"
            else:
                build_status = "No data"
        else:
            build_status = "No data"
    except Exception as e:
        build_status = "Query error"

    return build_status, url


exit_code = 0

for platform in platforms:
    header_printed = False

    if not failed_only:
        print_header(platform)
        header_printed = True

    # 1. First Pass: Check binary cache concurrently for packages that have a store path for this platform
    cached_status = {}  # pkg_name -> (is_cached: bool | None, narinfo_url)
    if use_cache:
        eligible = [
            p
            for p in pkgs
            if p.get("path")
            and (p.get("system") is None or p.get("system") == platform)
        ]
        if eligible:
            with concurrent.futures.ThreadPoolExecutor(
                max_workers=workers
            ) as executor:
                for name, is_cached, nar_url in executor.map(
                    check_cache_for_pkg, eligible
                ):
                    cached_status[name] = (is_cached, nar_url)

    # 2. Second Pass: Iterate packages in order, using cache hit or falling back to Hydra
    for pkg in pkgs:
        pkg_name = pkg["name"]
        is_cached, nar_url = cached_status.get(pkg_name, (None, None))

        if is_cached is True:
            build_status = "Succeeded"
            url = nar_url
        elif cache_only:
            build_status = "Not cached" if is_cached is False else "No store path"
            url = nar_url or f"{HYDRA_URL}/job/{project}/{jobset}/{pkg_name}.{platform}/all"
        else:
            # Fallback to Hydra
            build_status, url = query_hydra_status(pkg_name, platform)
            time.sleep(1)  # Only sleep when actually querying Hydra!

        is_ok = build_status in OK_STATUSES
        if not is_ok:
            exit_code = 1

        if not failed_only or not is_ok:
            if not header_printed:
                print_header(platform)
                header_printed = True
            print(f"| {pkg_name: <50} | {build_status: <20} | {url: <80} |")

sys.exit(exit_code)
