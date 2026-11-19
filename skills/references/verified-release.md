# Verified release procedure

Resolve the latest stable release from the approved upstream. For this checkout,
that is `https://github.com/ActionsCommunity/multirunner/releases/latest`. If the
configured remote or approved deployment source differs, stop and confirm the
release source rather than silently substituting a fork. Reject drafts,
prereleases, and assets for a different OS or architecture.

Download the matching binary and `SHA256SUMS.txt` over HTTPS to a temporary directory.
Verify the exact asset before execution:

- Windows: compare `Get-FileHash -Algorithm SHA256` with its checksum entry.
- Linux: run `sha256sum --check --ignore-missing SHA256SUMS.txt`.

Stop if the entry is absent or mismatched. The checksum entry is the asset's
identity. Tagged release binaries also embed the release tag and full source
commit, so run `multirunner --version` after checksum verification and require
both values to match the approved release tag and its resolved commit. Version
output is supporting provenance evidence, not a substitute for the checksum
or signed update metadata.

Show the destination, filesystem effects, and any required elevation before
approval. Keep the prior binary at a restrictive, versioned rollback path.
Preserve ownership and executable mode. Don't remove the prior binary or old
images.
