# Reaction artwork

From the repository root, run `./tools/reaction-artwork/generate.sh` to regenerate the static WebP, padded center-icon WebP, and TGS assets from Twemoji v14.0.2. The script verifies the upstream commit and uses the pinned SVG-to-Lottie and WebP converter dependencies.

Center icons keep a 512x512 canvas and place the visible Twemoji artwork inside a centered 256x256 area, with transparent padding around it. This gives the client a half-size emoji while preserving the canvas expected for reaction animations. Static reaction icons and TGS animations keep their existing artwork.

Generation requires Git, Node.js with npm, and Rust 1.85 or newer with Cargo.
