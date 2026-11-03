# Bundled Noto Emoji fallback

Linux images include the unmodified Noto Emoji variable TrueType font from
Google Fonts. The source is pinned to commit
`8b0a1d0f5983c89bc2b93f1b5fb55f9e252744b5`:

- [Font](https://raw.githubusercontent.com/google/fonts/8b0a1d0f5983c89bc2b93f1b5fb55f9e252744b5/ofl/notoemoji/NotoEmoji%5Bwght%5D.ttf)
- [Copyright and SIL Open Font License 1.1](https://raw.githubusercontent.com/google/fonts/8b0a1d0f5983c89bc2b93f1b5fb55f9e252744b5/ofl/notoemoji/OFL.txt)

The font is not sold separately or relicensed. Its original copyright and
complete license ship beside it. Only whitespace in the license text is
normalized for this repository; the font bytes are unchanged. Font SHA-256:
`de6c18832938afc99caf132b39d6a30a19bac7f2e812e28db2535b4608d27551`.

The assets live in `tools/linux/rootfs/usr/share/techo5/fonts`. The normal
rootfs build stages them without line-ending conversion. At startup,
`seed-fonts.sh` installs missing files into the device's persistent font folder;
it preserves existing files and symlinks, including owner-provided replacements.
This works on fresh installs and image upgrades, without runtime downloads.
The existing renderer refreshes its fallback fonts automatically.

The font supplies monochrome emoji glyphs; it does not add color-font rendering
or guarantee shaping of every combined emoji sequence. The normal interface
font remains unchanged.
