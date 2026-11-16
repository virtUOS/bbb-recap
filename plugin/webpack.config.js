// Builds dist/RecapPlugin.js (one file) and copies manifest.json next to it with the package version, which
// BBB appends to the script URL to bust browser caches (BBB docs/docs/plugins.md "version").
const CopyWebpackPlugin = require("copy-webpack-plugin");
const TerserPlugin = require("terser-webpack-plugin");
const { BannerPlugin } = require("webpack");
const { version } = require("./package.json");

// License notice at the top of RecapPlugin.js, so it travels with the file. Keep it in line with NOTICE: the packages
// webpack bundles (npx webpack --mode production --json, modules under node_modules/).
const tag = `https://github.com/virtUOS/bbb-recap/blob/v${version}`;
const banner = `bbb-recap BBB plugin ${version}, Apache License 2.0 (${tag}/LICENSE).
Bundles, unmodified:
- bigbluebutton-html-plugin-sdk 1.0.0-beta.3, GNU LGPL v3.0, source and license text:
  https://github.com/bigbluebutton/bigbluebutton-html-plugin-sdk/tree/v1.0.0-beta.3 (LICENSE.md; the LGPL
  supplements the GNU GPL v3.0, https://www.gnu.org/licenses/gpl-3.0.txt)
- react 18.3.1, react-dom 18.3.1, scheduler 0.23.2, MIT, Copyright (c) Facebook, Inc. and its affiliates
- browser-bunyan 1.8.0, @browser-bunyan/levels 1.8.0, @browser-bunyan/console-formatted-stream 1.8.0, MIT,
  Copyright (c) 2011-2012 Joyent Inc.
Notices, license texts and how to rebuild with a modified SDK: ${tag}/plugin/NOTICE`;

module.exports = {
  entry: "./src/index.tsx",
  output: {
    filename: "RecapPlugin.js",
    path: `${__dirname}/dist`,
    clean: true,
  },
  module: {
    rules: [{ test: /\.tsx?$/, use: "ts-loader", exclude: /node_modules/ }],
  },
  resolve: {
    extensions: [".ts", ".tsx", ".js"],
  },
  optimization: {
    // Keep the license comments in RecapPlugin.js instead of a separate .LICENSE.txt file.
    minimizer: [new TerserPlugin({ extractComments: false })],
  },
  performance: {
    // The bundle carries its own React and the SDK's GraphQL client.
    hints: false,
  },
  plugins: [
    new BannerPlugin({ banner }),
    new CopyWebpackPlugin({
      patterns: [
        {
          from: "manifest.json",
          transform: (content) => JSON.stringify({ ...JSON.parse(content.toString()), version }, null, 2) + "\n",
        },
      ],
    }),
  ],
};
