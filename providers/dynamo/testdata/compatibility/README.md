# Released Dynamo validation contracts

These JSON fixtures contain the complete OpenAPI/CEL schema for the tested API
version of each released CRD. Only `description`, `title`, `externalDocs`, and
`example` annotations are omitted. Properties, required fields, defaults, limits,
list semantics, and Kubernetes validation extensions are retained. Each fixture
records the immutable source commit, path, and SHA-256 of the original CRD.

Regenerate or verify from an existing Dynamo clone, without network access:

```sh
python3 hack/extract-contracts.py /path/to/dynamo
python3 hack/extract-contracts.py /path/to/dynamo --check
```

The generator requires PyYAML. Run it from any directory. Contract tests consume
only checked-in JSON and use the Kubernetes OpenAPI, CEL, and pruning validators.
They do not prove real operator execution, GPU performance, or gateway traffic.

The source releases are Dynamo 1.1.1 at
`f43ed79d4c3be0f27ca83b2fdf96bc580a2cd978` and Dynamo 1.5.0 at
`b83b1d9304ebfc624709ac46db32b1b6f1ff1615`.

The schemas are derived from NVIDIA Dynamo and remain under the Apache License
2.0. `UPSTREAM_LICENSE` preserves the source repository license and notices.
