use std::{env, fs, process};

use svg2lottie::{SvgToLottieOptions, convert_svg_to_tgs};

fn main() {
    if let Err(err) = run() {
        eprintln!("reaction artwork conversion failed: {err}");
        process::exit(1);
    }
}

fn run() -> Result<(), Box<dyn std::error::Error>> {
    let mut args = env::args_os().skip(1);
    let input = args
        .next()
        .ok_or("usage: reaction-artwork INPUT.svg OUTPUT.tgs")?;
    let output = args
        .next()
        .ok_or("usage: reaction-artwork INPUT.svg OUTPUT.tgs")?;
    if args.next().is_some() {
        return Err("usage: reaction-artwork INPUT.svg OUTPUT.tgs".into());
    }

    let svg = fs::read_to_string(input)?;
    let options = SvgToLottieOptions::telegram_sticker()
        .with_duration_frames(1)
        .with_name("Twemoji reaction");
    let tgs = convert_svg_to_tgs(&svg, &options)?;
    fs::write(output, tgs)?;
    Ok(())
}
