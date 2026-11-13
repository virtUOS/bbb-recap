// Builds dist/RecapPlugin.js (one file) and copies manifest.json next to it with the package version, which
// BBB appends to the script URL to bust browser caches (BBB docs/docs/plugins.md "version").
const CopyWebpackPlugin = require("copy-webpack-plugin");
const TerserPlugin = require("terser-webpack-plugin");
const { version } = require("./package.json");

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
