// SPDX-License-Identifier: AGPL-3.0-only

/**
 * What the panel does about each method of the API, one decision a method
 * (ADR 0059). methods.test.ts walks the generated services against it: a new
 * method fails there until it has a line here, and a line that says less than
 * the panel's source does fails too.
 */
export type PanelUse =
  /** Before sign-in, or the caller's own account: nothing to ask. */
  | "open"
  /** A read anyone with a role may make: the server narrows its list, or the screen shows its refusal. */
  | "read"
  /** The panel asks `may` for it by name before it offers it. */
  | "may"
  /** The plan says: what the sheet offers comes from the server's Change.waits and Change.approves (ADR 0049). */
  | "plan"
  /** Offered only on a screen whose way in asks `may` for the method named, which needs as much. */
  | { within: string }
  /** The panel doesn't call it. */
  | "unused";

export const PANEL_METHODS: Record<string, PanelUse> = {
  "AlertService.acknowledgeAlert": { within: "AlertService.listAlerts" },
  "AlertService.listAlertChecks": "may",
  "AlertService.listAlertChannels": { within: "AlertService.listAlerts" },
  "AlertService.listAlertDeliveries": { within: "AlertService.listAlerts" },
  "AlertService.listAlertMutes": "unused",
  "AlertService.listAlertRules": { within: "AlertService.listAlerts" },
  "AlertService.listAlerts": "may",
  "AlertService.muteAlerts": "unused",
  "AlertService.testAlertChannel": "may",
  "AlertService.unmuteAlerts": "unused",
  "AlertService.unacknowledgeAlert": { within: "AlertService.listAlerts" },

  "AuditService.listAuditEvents": "may",

  "AuthService.beginAddPasskey": "open",
  "AuthService.beginClaim": "open",
  "AuthService.beginPasskeySignIn": "open",
  "AuthService.beginStepUp": "open",
  "AuthService.beginTotpSetup": "open",
  "AuthService.cancelTotpSetup": "open",
  "AuthService.checkClaim": "open",
  "AuthService.confirmTotpSetup": "open",
  "AuthService.disableTotp": "open",
  "AuthService.finishAddPasskey": "open",
  "AuthService.finishClaim": "open",
  "AuthService.finishPasskeySignIn": "open",
  "AuthService.finishStepUp": "open",
  "AuthService.getAuthState": "open",
  "AuthService.getTotp": "open",
  "AuthService.listPasskeys": "open",
  "AuthService.listSessions": "open",
  "AuthService.regenerateRecoveryCodes": "open",
  "AuthService.removePasskey": "open",
  "AuthService.revokeOtherSessions": "open",
  "AuthService.revokeSession": "open",
  "AuthService.signInWithCode": "open",
  "AuthService.signInWithLink": "open",
  "AuthService.signOut": "open",
  "AuthService.unlockCodeSignIn": "open",

  "BackupService.createBackup": "may",
  "BackupService.deleteBackup": "may",
  "BackupService.fetchBackup": "may",
  "BackupService.getBackup": "unused",
  "BackupService.listBackupDestinations": "read",
  "BackupService.listBackupPolicies": "read",
  "BackupService.listBackups": "read",
  "BackupService.previewBackup": "read",
  "BackupService.restoreBackup": "may",
  "BackupService.testBackupDestination": "may",

  "CatalogService.getCatalogEntry": "read",
  "CatalogService.installCatalogEntry": "may",
  "CatalogService.listCatalogEntries": "read",
  "CatalogService.listCatalogInstalls": "read",
  "CatalogService.listCatalogSourceRefs": "unused",
  "CatalogService.listCatalogSources": "unused",
  "CatalogService.removeCatalogSource": "unused",
  "CatalogService.setCatalogSource": "unused",
  "CatalogService.upgradeCatalogInstall": "may",

  "ChangeService.adoptDrift": "unused",
  "ChangeService.applyChange": "plan",
  "ChangeService.confirmChange": "may",
  "ChangeService.discardChange": "plan",
  "ChangeService.getChange": "read",
  "ChangeService.listChanges": "read",
  "ChangeService.listDrift": "unused",
  "ChangeService.proposeChange": "may",
  "ChangeService.restartService": "may",
  "ChangeService.revertDrift": "unused",
  "ChangeService.undoChange": "may",
  "ChangeService.watchChange": "read",

  "ComposeService.adoptCompose": { within: "ContainerService.listContainers" },
  "ComposeService.checkCompose": "may",
  "ComposeService.exportCompose": "read",
  "ComposeService.importCompose": "may",

  "ContainerService.adoptContainer": { within: "ContainerService.listContainers" },
  "ContainerService.listContainers": "may",

  "DatabaseService.changeDatabasePassword": "may",
  "DatabaseService.createDatabaseUser": "may",
  "DatabaseService.getDatabase": "read",
  "DatabaseService.listDatabases": "read",

  "DomainService.checkDomain": "may",
  "DomainService.listDomains": "read",

  "EventService.listEvents": "read",
  "EventService.watchEvents": "read",

  "FileService.beginUpload": { within: "FileService.listFiles" },
  "FileService.cancelUpload": { within: "FileService.listFiles" },
  "FileService.deleteFile": { within: "FileService.listFiles" },
  // Its bytes go over the REST path, from the Files screen.
  "FileService.downloadFile": { within: "FileService.listFiles" },
  "FileService.finishUpload": { within: "FileService.listFiles" },
  "FileService.listDeletedFiles": { within: "FileService.listFiles" },
  "FileService.listFiles": "may",
  "FileService.makeFile": { within: "FileService.listFiles" },
  "FileService.moveFile": { within: "FileService.listFiles" },
  "FileService.purgeDeletedFile": { within: "FileService.listFiles" },
  "FileService.readFile": { within: "FileService.listFiles" },
  "FileService.restoreFile": { within: "FileService.listFiles" },
  "FileService.saveFile": { within: "FileService.listFiles" },
  // Its bytes go over the REST path, from the Files screen.
  "FileService.uploadFile": { within: "FileService.listFiles" },

  "FirewallService.getServerFirewall": "may",

  "HealthService.listHealth": "read",

  "ImageService.getImage": "may",
  "ImageService.listImageTags": "may",
  "ImageService.searchImages": "may",

  "ImportService.importOnePanel": "may",

  "LogService.listLogSources": "read",
  "LogService.readLogs": "read",
  "LogService.watchLogs": "read",

  "MemberService.listMembers": "may",

  "ProcessService.endProcess": { within: "ProcessService.watchProcesses" },
  "ProcessService.watchProcesses": "may",

  "ResourceService.listResources": "read",

  "ServerService.createJoinTicket": "may",
  "ServerService.getJoinTicket": { within: "ServerService.createJoinTicket" },
  "ServerService.getServer": "unused",
  "ServerService.checkServerPorts": "may",
  "ServerService.getServerDiskUse": "may",
  "ServerService.listServers": "unused",
  "ServerService.setServerPublicAddress": "may",
  "ServerService.watchServers": "read",

  "ServiceService.addServicePort": "may",
  "ServiceService.deployService": "may",
  "ServiceService.listServiceRevisions": "read",
  "ServiceService.listServices": "read",
  "ServiceService.openService": "may",
  "ServiceService.rollBackService": "may",

  "SettingsService.getControlPlane": "may",
  "SettingsService.getSettings": "read",
  "SettingsService.setInstallName": "may",
  "SettingsService.setTimeZone": "may",

  "SystemService.getVersion": "open",

  "TaskService.getTask": "unused",
  "TaskService.listTaskRuns": "read",
  "TaskService.listTasks": "read",
  "TaskService.runTask": "may",

  "TerminalService.attachTerminal": { within: "TerminalService.listTerminals" },
  "TerminalService.closeTerminal": { within: "TerminalService.listTerminals" },
  "TerminalService.listTerminals": "may",
  "TerminalService.openTerminal": { within: "TerminalService.listTerminals" },
  "TerminalService.writeTerminal": { within: "TerminalService.listTerminals" },

  "TokenService.createToken": "open",
  "TokenService.listTokens": "open",
  "TokenService.revokeToken": "open",

  "UpdateService.checkServerUpdates": { within: "UpdateService.getServerUpdates" },
  "UpdateService.getServerUpdates": "may",

  "VariableService.listVariables": "read",
  "VariableService.revealSecret": "may",

  "WebsiteService.createWebsite": "may",
  "WebsiteService.deleteWebsite": "may",
  "WebsiteService.listWebsiteRuntimes": "read",
  "WebsiteService.listWebsites": "read",
  "WebsiteService.previewWebsite": "read",
  "WebsiteService.updateWebsite": "may",
};
