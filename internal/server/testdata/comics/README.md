# Synthetic comic fixtures

All content was generated for these tests: a 2x2 red/blue PNG and a short
ComicInfo.xml with title Archive Example, writer Ada Example, and series
Example Series. No user books or downloaded copyrighted pages are included.

- rar4.cbr and rar5.cbr: stored RAR4/RAR5 headers generated from the documented
  file format with CRC32 checksums. Also independently extracted by 7-Zip 24.09.
- unsafe.cbr: stored RAR4 with the image named ../001.png.
- lzma.cb7, lzma2.cb7, copy.cb7: 7-Zip 24.09 archives made with respectively
  `7z a -t7z -m0=lzma`, `-m0=lzma2`, and `-m0=copy`.
- encrypted.cb7: 7-Zip archive with `-pfixture-password -mhe=on`.
- empty.cb7: contains only ComicInfo.xml; it is not a comic.
- large-dictionary.cb7: LZMA2 with uncompressed headers (`-mhc=off`); the
  dictionary property was changed to 30 (128 MiB), then both header CRCs were
  recalculated. It must be rejected before allocating that dictionary.

Positive fixtures exercise actual decoder calls in content detection, metadata,
uploads, watched folders, downloads, and backup/restore tests.
