#!/usr/bin/env python3
"""Extract released validation contracts. Requires PyYAML; never uses the network."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import yaml

TARGETS = [
    ("v1.1.1", "f43ed79d4c3be0f27ca83b2fdf96bc580a2cd978", "dynamographdeployments", "v1alpha1", "9b855943c77833e931441fb20f95d5944958e5dcfc339c3ec26506df48361410"),
    ("v1.1.1", "f43ed79d4c3be0f27ca83b2fdf96bc580a2cd978", "dynamographdeploymentrequests", "v1beta1", "6bfbef2ee540513fb308b41d3c7c048fbfafffb6b577356dd253a4f5ff5150cc"),
    ("v1.5.0", "b83b1d9304ebfc624709ac46db32b1b6f1ff1615", "dynamographdeployments", "v1beta1", "afb5fb9f2b515c4e865024f6441195fd081dce2e5b83c25440dce50531c602b9"),
    ("v1.5.0", "b83b1d9304ebfc624709ac46db32b1b6f1ff1615", "dynamographdeployments", "v1alpha1", "afb5fb9f2b515c4e865024f6441195fd081dce2e5b83c25440dce50531c602b9"),
    ("v1.5.0", "b83b1d9304ebfc624709ac46db32b1b6f1ff1615", "dynamographdeploymentrequests", "v1beta1", "d840473b5260a2b2e40ff69d3284ee8d9dc7af94506c8e7e864f1ca407575245"),
]


def without_prose(schema):
    """Do not descend into default/enum values or delete names in properties."""
    if isinstance(schema, list):
        return [without_prose(item) for item in schema]
    if not isinstance(schema, dict):
        return schema
    out = dict(schema)
    for key in ("description", "title", "externalDocs", "example"):
        out.pop(key, None)
    for key in ("properties", "patternProperties", "definitions", "$defs", "dependencies"):
        if isinstance(out.get(key), dict):
            out[key] = {name: without_prose(value) for name, value in out[key].items()}
    for key in ("items", "additionalProperties", "additionalItems", "not", "allOf", "anyOf", "oneOf"):
        if key in out:
            out[key] = without_prose(out[key])
    return out


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("upstream", type=Path)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    destination = Path(__file__).resolve().parent.parent / "testdata" / "compatibility"
    for release, commit, resource, version, expected in TARGETS:
        source = f"deploy/operator/config/crd/bases/nvidia.com_{resource}.yaml"
        raw = subprocess.check_output(["git", "-C", str(args.upstream), "show", f"{commit}:{source}"])
        digest = hashlib.sha256(raw).hexdigest()
        if digest != expected:
            raise ValueError(f"unexpected source checksum for {release}/{resource}")
        crd = yaml.safe_load(raw)
        selected = next(item for item in crd["spec"]["versions"] if item["name"] == version)
        if not selected["served"]:
            raise ValueError(f"{version} is not served")
        fixture = {"release": release, "commit": commit, "source": source,
                   "sourceSha256": digest, "apiVersion": version,
                   "schema": without_prose(selected["schema"]["openAPIV3Schema"])}
        text = json.dumps(fixture, indent=2, sort_keys=True) + "\n"
        path = destination / f"{release}-{resource}-{version}.json"
        if args.check:
            if path.read_text() != text:
                raise ValueError(f"fixture differs: {path}")
        else:
            path.write_text(text)
        print(path.name)


if __name__ == "__main__":
    main()
