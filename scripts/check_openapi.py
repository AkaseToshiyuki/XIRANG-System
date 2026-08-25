#!/usr/bin/env python3
"""Perform deterministic structural checks for the checked-in OpenAPI contract."""

from pathlib import Path
import sys

import yaml


class StrictLoader(yaml.SafeLoader):
    """YAML loader that rejects keys which PyYAML would otherwise overwrite."""


def construct_unique_mapping(loader: StrictLoader, node: yaml.MappingNode, deep: bool = False) -> dict:
    result = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if key in result:
            raise ValueError(f"duplicate key {key!r} at line {key_node.start_mark.line + 1}")
        result[key] = loader.construct_object(value_node, deep=deep)
    return result


StrictLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG,
    construct_unique_mapping,
)


def resolve_local_reference(document: dict, reference: str) -> None:
    current = document
    for encoded_part in reference[2:].split("/"):
        part = encoded_part.replace("~1", "/").replace("~0", "~")
        if not isinstance(current, dict) or part not in current:
            raise ValueError(f"unresolved local reference: {reference}")
        current = current[part]


def validate(document: dict) -> None:
    if document.get("openapi") != "3.1.0":
        raise ValueError("openapi must be 3.1.0")
    if not document.get("info", {}).get("version") or not document.get("paths"):
        raise ValueError("info.version and paths are required")
    if not document.get("components", {}).get("schemas"):
        raise ValueError("components.schemas is required")

    operation_ids: set[str] = set()

    def visit(value: object) -> None:
        if isinstance(value, dict):
            reference = value.get("$ref")
            if isinstance(reference, str) and reference.startswith("#/"):
                resolve_local_reference(document, reference)
            operation_id = value.get("operationId")
            if isinstance(operation_id, str):
                if operation_id in operation_ids:
                    raise ValueError(f"duplicate operationId: {operation_id}")
                operation_ids.add(operation_id)
            for child in value.values():
                visit(child)
        elif isinstance(value, list):
            for child in value:
                visit(child)

    visit(document)


def main() -> int:
    path = Path(sys.argv[1] if len(sys.argv) > 1 else "api/openapi/xirang-v1.yaml")
    try:
        document = yaml.load(path.read_text(encoding="utf-8"), Loader=StrictLoader)
        if not isinstance(document, dict):
            raise ValueError("contract root must be an object")
        validate(document)
    except (OSError, yaml.YAMLError, ValueError) as error:
        print(f"OpenAPI contract invalid: {error}", file=sys.stderr)
        return 1
    print(
        f"OpenAPI contract parsed: {len(document['paths'])} paths, "
        f"{len(document['components']['schemas'])} schemas"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
