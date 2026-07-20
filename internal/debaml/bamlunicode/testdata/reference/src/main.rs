//! Exact BAML 0.223.0 CFFI Unicode oracle.
//!
//! This binary is the primitive oracle for `internal/debaml/bamlunicode`. It is
//! built with the pinned BAML build toolchain (rustc 1.93.0 -> Unicode 17.0.0)
//! and the pinned normalization crate (unicode-normalization 0.1.24 -> Unicode
//! 16.0.0), and exposes exactly the primitives BAML's match_string path uses:
//!
//!   * str::to_lowercase                       (Unicode 17.0.0, incl. Final_Sigma)
//!   * char::is_alphanumeric                   (Unicode 17.0.0)
//!   * char::is_whitespace / str::trim         (Unicode 17.0.0)
//!   * UnicodeNormalization::nfkd              (Unicode 16.0.0)
//!   * unicode_normalization::char::is_combining_mark (Unicode 16.0.0)
//!
//! It never enters the Go build. The Go differential test (build tag
//! `unicode_rustref`) compiles this with `cargo +1.93.0` and compares UTF-8
//! bytes for every scalar and for every generated context.
//!
//! Subcommands:
//!   versions       -> "char=<v>\nnorm=<v>\n"
//!   dump-scalars   -> one record per scalar 0..=0x10FFFF (surrogates skipped):
//!                     "<cp:X> <lower:hexbytes> <nfkd:hexbytes> <flags:X>\n"
//!                     flags: bit0 is_alphanumeric, bit1 is_whitespace,
//!                            bit2 is_combining_mark, bit3 Cased-via-Final_Sigma
//!                            (X+Σ), bit4 Case_Ignorable/Cased-via-Final_Sigma
//!                            (a+X+Σ), bit5 Cased, bit6 Case_Ignorable.
//!                     dump-scalars requires the path to UCD 17.0.0
//!                     DerivedCoreProperties.txt as argv[2]; the Cased /
//!                     Case_Ignorable bits are an INDEPENDENT parse of that
//!                     authoritative source (the same file rustc std's tables
//!                     are generated from), so bit5/bit6 prove the vendored
//!                     tables' membership directly and unambiguously for every
//!                     scalar — including the Cased ∩ Case_Ignorable overlap the
//!                     Final_Sigma bits cannot disambiguate.
//!   strmap         -> stdin lines "L <hexutf8>" / "K <hexutf8>";
//!                     stdout "<hexutf8-result>\n" (L=lowercase, K=nfkd)

use std::io::{self, BufRead, Read, Write};

use unicode_normalization::char::is_combining_mark;
use unicode_normalization::UnicodeNormalization;

fn to_hex(bytes: &[u8]) -> String {
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        s.push(char::from_digit((b >> 4) as u32, 16).unwrap());
        s.push(char::from_digit((b & 0xf) as u32, 16).unwrap());
    }
    s
}

fn from_hex(s: &str) -> Vec<u8> {
    let b = s.trim().as_bytes();
    assert!(b.len().is_multiple_of(2), "hex input must have even length");
    let mut out = Vec::with_capacity(b.len() / 2);
    for pair in b.chunks_exact(2) {
        let hi = (pair[0] as char).to_digit(16).unwrap();
        let lo = (pair[1] as char).to_digit(16).unwrap();
        out.push(((hi << 4) | lo) as u8);
    }
    out
}

fn versions() {
    let c = char::UNICODE_VERSION;
    let n = unicode_normalization::UNICODE_VERSION;
    print!("char={}.{}.{}\nnorm={}.{}.{}\n", c.0, c.1, c.2, n.0, n.1, n.2);
}

// parse_dcp independently parses UCD DerivedCoreProperties.txt and returns
// per-scalar Cased and Case_Ignorable membership. This is the authoritative
// source rustc std's own Cased/Case_Ignorable tables are generated from, so it
// is a valid independent oracle for the vendored tables (and, unlike the
// Final_Sigma probes, it distinguishes every combination of the two properties).
fn parse_dcp(path: &str) -> (Vec<bool>, Vec<bool>) {
    let mut cased = vec![false; 0x110000];
    let mut case_ignorable = vec![false; 0x110000];
    let text = std::fs::read_to_string(path).expect("read DerivedCoreProperties.txt");
    for raw in text.lines() {
        let line = raw.split('#').next().unwrap_or("").trim();
        if line.is_empty() {
            continue;
        }
        let mut parts = line.split(';');
        let range = parts.next().unwrap_or("").trim();
        let prop = match parts.next() {
            Some(p) => p.trim(),
            None => continue,
        };
        let target = match prop {
            "Cased" => &mut cased,
            "Case_Ignorable" => &mut case_ignorable,
            _ => continue,
        };
        let (lo, hi) = parse_dcp_range(range);
        for cp in lo..=hi {
            target[cp as usize] = true;
        }
    }
    (cased, case_ignorable)
}

fn parse_dcp_range(s: &str) -> (u32, u32) {
    if let Some(idx) = s.find("..") {
        let lo = u32::from_str_radix(&s[..idx], 16).unwrap();
        let hi = u32::from_str_radix(s[idx + 2..].trim(), 16).unwrap();
        (lo, hi)
    } else {
        let v = u32::from_str_radix(s, 16).unwrap();
        (v, v)
    }
}

fn dump_scalars(dcp_path: &str) {
    let (cased, case_ignorable) = parse_dcp(dcp_path);
    let stdout = io::stdout();
    let mut out = io::BufWriter::with_capacity(1 << 20, stdout.lock());
    let mut line = String::with_capacity(64);
    for cp in 0u32..=0x10FFFF {
        // Skip UTF-16 surrogate range: not scalar values.
        if (0xD800..=0xDFFF).contains(&cp) {
            continue;
        }
        let c = char::from_u32(cp).unwrap();

        let lower: String = c.to_lowercase().collect();
        let nfkd: String = c.nfkd().collect();

        let mut flags: u32 = 0;
        if c.is_alphanumeric() {
            flags |= 1;
        }
        if c.is_whitespace() {
            flags |= 2;
        }
        if is_combining_mark(c) {
            flags |= 4;
        }

        // Cased / Case_Ignorable are not public in std, but they are consumed
        // ONLY by str::to_lowercase's Final_Sigma context. We probe them
        // per-scalar through that sole observable channel:
        //   bit3: to_lowercase(X + "Σ")     ends in ς  <=>  Cased(X) && !Case_Ignorable(X)
        //   bit4: to_lowercase("a" + X + "Σ") ends in ς  <=>  Case_Ignorable(X) || Cased(X)
        // (with a definitely-Cased, definitely-not-Case_Ignorable 'a' before X).
        // Together these pin, for every scalar, the exact Cased/Case_Ignorable
        // behavior the matcher can observe.
        const SIGMA: char = '\u{03A3}';
        const FINAL_SIGMA: char = '\u{03C2}';
        let mut s1 = String::with_capacity(8);
        s1.push(c);
        s1.push(SIGMA);
        if s1.to_lowercase().ends_with(FINAL_SIGMA) {
            flags |= 8;
        }
        let mut s2 = String::with_capacity(8);
        s2.push('a');
        s2.push(c);
        s2.push(SIGMA);
        if s2.to_lowercase().ends_with(FINAL_SIGMA) {
            flags |= 16;
        }

        // Raw Cased / Case_Ignorable membership from the independent DCP parse.
        // bit5/bit6 disambiguate every (Cased, Case_Ignorable) combination,
        // including the overlap the Final_Sigma bits alone cannot.
        if cased[cp as usize] {
            flags |= 32;
        }
        if case_ignorable[cp as usize] {
            flags |= 64;
        }

        line.clear();
        line.push_str(&format!(
            "{:X} {} {} {:X}\n",
            cp,
            to_hex(lower.as_bytes()),
            to_hex(nfkd.as_bytes()),
            flags
        ));
        out.write_all(line.as_bytes()).unwrap();
    }
    out.flush().unwrap();
}

fn strmap() {
    let stdin = io::stdin();
    let stdout = io::stdout();
    let mut out = io::BufWriter::new(stdout.lock());
    for line in stdin.lock().lines() {
        let line = line.unwrap();
        let line = line.trim();
        if line.is_empty() {
            continue;
        }
        let (mode, rest) = line.split_at(1);
        let bytes = from_hex(rest);
        let input = String::from_utf8(bytes).expect("input must be valid UTF-8");
        let result: String = match mode {
            "L" => input.to_lowercase(),
            "K" => input.nfkd().collect(),
            "T" => input.trim().to_string(),
            other => panic!("unknown mode {other}"),
        };
        out.write_all(to_hex(result.as_bytes()).as_bytes()).unwrap();
        out.write_all(b"\n").unwrap();
    }
    out.flush().unwrap();
}

fn main() {
    let arg = std::env::args().nth(1).unwrap_or_default();
    match arg.as_str() {
        "versions" => versions(),
        "dump-scalars" => {
            let dcp = std::env::args().nth(2).unwrap_or_else(|| {
                eprintln!("dump-scalars requires the path to DerivedCoreProperties.txt");
                std::process::exit(2);
            });
            dump_scalars(&dcp);
        }
        "strmap" => strmap(),
        _ => {
            // Drain stdin so a mis-drive doesn't deadlock a pipe on the Go side.
            let mut _sink = Vec::new();
            let _ = io::stdin().read_to_end(&mut _sink);
            eprintln!("usage: bamlunicode-reference <versions|dump-scalars <dcp-path>|strmap>");
            std::process::exit(2);
        }
    }
}
