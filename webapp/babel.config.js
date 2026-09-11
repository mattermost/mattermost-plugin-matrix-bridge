// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

// @emotion/babel-preset-css-prop is deliberately not included: no component in
// this codebase uses the css prop, and the preset's automatic-JSX-runtime rewrite
// (importing every element factory from @emotion/react instead of react) makes
// @emotion/react's precompiled bundle throw ("React.createContext is not a
// function") the moment anything actually renders under Jest + jsdom - a gap this
// config never hit before @testing-library/react was added, since no prior test
// rendered a component. Babel's `env` key merges onto, rather than replacing, the
// top-level config, so this preset had to be removed here rather than filtered
// out of config.env.test alone.
const config = {
    presets: [
        ['@babel/preset-env', {
            targets: {
                chrome: 66,
                firefox: 60,
                edge: 42,
                safari: 12,
            },
            modules: false,
            corejs: 3,
            debug: false,
            useBuiltIns: 'usage',
            shippedProposals: true,
        }],
        ['@babel/preset-react', {
            useBuiltIns: true,
        }],
        ['@babel/typescript', {
            allExtensions: true,
            isTSX: true,
        }],
    ],
    plugins: [
        'babel-plugin-typescript-to-proptypes',
    ],
};

// Jest needs module transformation
config.env = {
    test: {
        presets: config.presets,
        plugins: config.plugins,
    },
};
config.env.test.presets[0][1].modules = 'auto';

module.exports = config;
