# Verified release procedure

Resolve the latest stable release from
`https://github.com/ActionsCommunity/multirunner/releases/latest`. Reject drafts,
prereleases, and assets for a different OS or architecture.

Download the matching binary and `SHA256SUMS.txt` over HTTPS to a temporary directory.
Verify the exact asset before execution:

- Windows: compare `Get-FileHash -Algorithm SHA256` with its checksum entry.
- Linux: run `sha256sum --check --ignore-missing SHA256SUMS.txt`.

Stop if the entry is absent or mismatched. Confirm the staged binary reports the
expected version. Show the destination, filesystem effects, and any required elevation
before approval. Keep the prior binary at a restrictive, versioned rollback path.
Preserve ownership and executable mode. Don't remove the prior binary or old images.
