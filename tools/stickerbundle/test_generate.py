import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

import generate


class EmojiDefinitionTests(unittest.TestCase):
    def test_only_fully_qualified_default_skin_emojis_are_selected(self):
        source = """# group: Smileys & Emotion
1F600 ; fully-qualified     # 😀 E1.0 grinning face
1F600 ; minimally-qualified # 😀 E1.0 grinning face
1F44D 1F3FD ; fully-qualified # 👍🏽 E1.0 thumbs up: medium skin tone
# group: Food & Drink
1F34D ; fully-qualified # 🍍 E0.6 pineapple
"""

        result = generate.parse_emoji_test(source, {"Smileys & Emotion"})

        self.assertEqual([(emoji.codepoints, emoji.text) for emoji in result], [((0x1F600,), "😀")])

    def test_group_larger_than_120_splits_into_numbered_packs(self):
        emoji_group = [generate.Emoji((0x1F600 + offset,), chr(0x1F600 + offset)) for offset in range(121)]

        packs = generate.make_packs(
            source_id="twemoji",
            source_name="Twemoji",
            license_id="CC-BY-4.0",
            group_name="Smileys & Emotion",
            emojis=emoji_group,
            available={emoji.codepoints for emoji in emoji_group},
        )

        self.assertEqual([pack.short_name for pack in packs], ["twemoji_smileys_emotion_01", "twemoji_smileys_emotion_02"])
        self.assertEqual([len(pack.stickers) for pack in packs], [120, 1])

    def test_source_asset_lookup_prefers_raw_zwj_filename_then_falls_back(self):
        codepoints = (0x1F937, 0x200D, 0x2640, 0xFE0F)
        source_files = (
            (
                {"name": "Twemoji", "kind": "twemoji"},
                "assets/svg/1f937-200d-2640-fe0f.svg",
                "assets/svg/1f937-200d-2640.svg",
            ),
            (
                {"name": "OpenMoji", "kind": "openmoji"},
                "color/svg/1F937-200D-2640-FE0F.svg",
                "color/svg/1F937-200D-2640.svg",
            ),
        )

        for source, raw_path, normalized_path in source_files:
            with self.subTest(source=source["name"]):
                self.assertEqual(generate.source_asset_path(source, codepoints, {raw_path: {}}), raw_path)
                self.assertEqual(
                    generate.source_asset_path(source, codepoints, {normalized_path: {}}),
                    normalized_path,
                )
                self.assertEqual(
                    generate.source_asset_path(source, codepoints, {raw_path: {}, normalized_path: {}}),
                    raw_path,
                )

    def test_source_digest_changes_when_downloaded_content_changes(self):
        first = {"assets/a.svg": b"first", "LICENSE": b"license"}
        same = {"LICENSE": b"license", "assets/a.svg": b"first"}
        changed = {"LICENSE": b"license", "assets/a.svg": b"tampered"}

        self.assertEqual(generate.source_digest(first), generate.source_digest(same))
        self.assertNotEqual(generate.source_digest(first), generate.source_digest(changed))

    def test_tampered_source_digest_error_names_source(self):
        verify = getattr(generate, "verify_source_checksum", None)
        self.assertIsNotNone(verify)

        with self.assertRaisesRegex(
            generate.GeneratorError,
            "Noto Emoji source download checksum mismatch",
        ):
            verify("Noto Emoji", "0" * 64, {"2D/svg/emoji_u1f600.svg": b"tampered"})


class FluentMetadataTests(unittest.TestCase):
    def test_metadata_uses_default_color_art_when_direct_color_is_absent(self):
        metadata_path = "assets/waving-hand/metadata.json"
        art_path = "assets/waving-hand/Default/Color/waving-hand.svg"
        files = {metadata_path: b'{"unicode":"1F44B"}'}
        tree = {metadata_path: {}, art_path: {}}

        index = generate.parse_fluent_metadata(
            {"name": "Microsoft Fluent Emoji"}, files, tree,
        )

        self.assertEqual(index, {(0x1F44B,): art_path})

    def test_metadata_rejects_ambiguous_default_color_art(self):
        metadata_path = "assets/waving-hand/metadata.json"
        files = {metadata_path: b'{"unicode":"1F44B"}'}
        tree = {
            metadata_path: {},
            "assets/waving-hand/Default/Color/waving-hand.svg": {},
            "assets/waving-hand/Default/Color/waving-hand_variant.svg": {},
        }

        with self.assertRaisesRegex(
            generate.GeneratorError,
            "Microsoft Fluent Emoji source metadata assets/waving-hand/metadata.json has ambiguous Color art",
        ):
            generate.parse_fluent_metadata(
                {"name": "Microsoft Fluent Emoji"}, files, tree,
            )


if __name__ == "__main__":
    unittest.main()
