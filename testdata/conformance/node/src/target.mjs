export function formatTarget(repository, platform) {
  if (!/^[A-Za-z0-9][A-Za-z0-9_.-]*\/[A-Za-z0-9][A-Za-z0-9_.-]*$/.test(repository)) {
    throw new TypeError("repository must use owner/name format");
  }
  if (platform !== "linux" && platform !== "windows") {
    throw new TypeError("platform must be linux or windows");
  }
  return `${repository}:${platform}`;
}
