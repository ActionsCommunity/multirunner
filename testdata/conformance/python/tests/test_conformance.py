import unittest

from conformance import fixture_digest


class FixtureDigestTests(unittest.TestCase):
    def test_returns_stable_sha256(self) -> None:
        self.assertEqual(
            fixture_digest("ActionsCommunity/multirunner", "linux"),
            "0ac05a6517f98708fc33529a07e6536723d5bf7007386a3e3f0f1b535108a7f2",
        )

    def test_rejects_invalid_repository(self) -> None:
        with self.assertRaisesRegex(ValueError, "owner/name"):
            fixture_digest("multirunner", "linux")

    def test_rejects_unsupported_platform(self) -> None:
        with self.assertRaisesRegex(ValueError, "unsupported"):
            fixture_digest("ActionsCommunity/multirunner", "macos")


if __name__ == "__main__":
    unittest.main()
