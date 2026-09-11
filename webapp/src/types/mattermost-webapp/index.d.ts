// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

export interface PluginRegistry {
    registerPostTypeComponent(typeName: string, component: React.ElementType);
    registerAdminConsoleCustomSetting(key: string, component: React.ElementType, options?: {showTitle?: boolean});

    // The console renders a registered section's component with
    // {settingsList, sectionTitle, sectionDescription} (schema_admin_settings.tsx,
    // the section.component branch). key must match a lowercased "key" in
    // plugin.json's settings_schema.sections - the plugins reducer and
    // custom_plugin_settings both lowercase it on registration and lookup.
    registerAdminConsoleCustomSection(key: string, component: React.ElementType);

    // Add more if needed from https://developers.mattermost.com/extend/plugins/webapp/reference
}
