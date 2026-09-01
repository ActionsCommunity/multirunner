import hashlib


def fixture_digest(repository: str, platform: str) -> str:
    if repository.count("/") != 1:
        raise ValueError("repository must use owner/name format")
    if platform not in {"linux", "windows"}:
        raise ValueError("unsupported platform")
    value = f"{repository}:{platform}".encode()
    return hashlib.sha256(value).hexdigest()
