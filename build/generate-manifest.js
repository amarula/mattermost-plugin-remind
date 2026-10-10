#!/usr/bin/env node
// Generates the plugin manifest for one platform's bundle.
//
// The tracked plugin.json lists every platform the plugin is built for, because
// that is what the repository says it supports. Each tarball carries exactly one
// of those executables, so its manifest has to advertise exactly that one: the
// server reads the map to find the binary to run, and a bundle that names a
// binary it does not contain will not load.
//
// usage: generate-manifest.js <manifest> <out> <platform>

const fs = require('fs');

const args = process.argv.slice(2);
if (args.length !== 3) {
    console.error('usage: generate-manifest.js <manifest> <out> <platform>');
    process.exit(1);
}

const [manifestPath, outPath, platform] = args;

const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));

const executable = (manifest.server || {}).executables ? manifest.server.executables[platform] : undefined;
if (!executable) {
    console.error(`${manifestPath} has no "server.executables" entry for platform ${platform}`);
    process.exit(1);
}

manifest.server = {executables: {[platform]: executable}};

fs.writeFileSync(outPath, `${JSON.stringify(manifest, null, 2)}\n`, 'utf8');
console.log(`Wrote ${outPath} for ${platform} (${executable})`);
