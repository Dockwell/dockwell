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
  "AuthService.beginAcceptInvite": "open",
  "AuthService.beginClaim": "open",
  "AuthService.beginPasskeySignIn": "open",
  "AuthService.beginStepUp": "open",
  "AuthService.beginTotpSetup": "open",
  "AuthService.cancelTotpSetup": "open",
  "AuthService.checkClaim": "open",
  "AuthService.checkInvite": "open",
  "AuthService.confirmTotpSetup": "open",
  "AuthService.disableTotp": "open",
  "AuthService.finishAddPasskey": "open",
  "AuthService.finishAcceptInvite": "open",
  "AuthService.finishClaim": "open",
  "AuthService.finishPasskeySignIn": "open",
  "AuthService.finishStepUp": "open",
  "AuthService.getAuthState": "open",
  "AuthService.getStepUpPolicy": "open",
  "AuthService.getTotp": "open",
  "AuthService.listPasskeys": "open",
  "AuthService.listSessions": "open",
  "AuthService.regenerateRecoveryCodes": "open",
  "AuthService.removePasskey": "open",
  "AuthService.revokeOtherSessions": "open",
  "AuthService.revokeSession": "open",
  "AuthService.setStepUpPolicy": "open",
  "AuthService.signInWithCode": "open",
  "AuthService.signInWithLink": "open",
  "AuthService.signOut": "open",
  "AuthService.unlockCodeSignIn": "open",

  "BackupService.createBackup": "may",
  "BackupService.deleteBackup": "may",
  "BackupService.drillBackup": "may",
  "BackupService.fetchBackup": "may",
  "BackupService.getBackup": "read",
  "BackupService.listBackupDestinations": "read",
  "BackupService.listBackupPolicies": "read",
  "BackupService.listBackups": "read",
  "BackupService.previewBackup": "read",
  "BackupService.restoreBackup": "may",
  "BackupService.testBackupDestination": "may",

  "BuildService.cancelBuild": "may",
  "BuildService.detectBuild": "may",
  "BuildService.getBuild": "unused",
  "BuildService.listBuilds": "unused",
  "BuildService.listRepositoryBranches": "may",
  "BuildService.readBuildLog": "read",
  "BuildService.watchBuild": "read",

  "CatalogService.checkCatalogSource": "may",
  "CatalogService.getCatalogEntry": "read",
  "CatalogService.installCatalogEntry": "may",
  "CatalogService.listCatalogEntries": "read",
  "CatalogService.listCatalogInstalls": "read",
  "CatalogService.listCatalogSourceRefs": { within: "CatalogService.setCatalogSource" },
  "CatalogService.listCatalogSources": "read",
  "CatalogService.listCatalogStores": "read",
  "CatalogService.backUpCatalogNeed": "may",
  "CatalogService.listCatalogNeedBackups": "read",
  "CatalogService.openCatalogInstall": "may",
  "CatalogService.restoreCatalogNeed": "may",
  "CatalogService.removeCatalogSource": "may",
  "CatalogService.setCatalogCompanion": "may",
  "CatalogService.setCatalogNeed": "may",
  "CatalogService.setCatalogSource": "may",
  "CatalogService.upgradeCatalogInstall": "may",

  "ChangeService.adoptDrift": "unused",
  "ChangeService.applyChange": "plan",
  "ChangeService.confirmChange": "may",
  "ChangeService.discardChange": "plan",
  "ChangeService.getChange": "read",
  "ChangeService.listChanges": "read",
  "ChangeService.listDrift": "unused",
  "ChangeService.proposeChange": "may",
  "ChangeService.planInstallUpdates": "may",
  "ChangeService.planReboot": "may",
  "ChangeService.planRemoveServer": "may",
  "ChangeService.planServerLabels": "may",
  "ChangeService.planSystemServiceAction": { within: "ServerService.watchSystemServices" },
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

  "DomainService.checkDnsProvider": "may",
  "DomainService.listCertificates": "unused",
  "DomainService.checkDomain": "may",
  "DomainService.listDnsProviders": "read",
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
  // The SSH and fail2ban cards are on the Firewall tab, offered where its read is.
  "FirewallService.getServerFail2ban": { within: "FirewallService.getServerFirewall" },
  "FirewallService.getServerSsh": { within: "FirewallService.getServerFirewall" },

  "HealthService.listHealth": "read",

  "ImageService.checkRegistryLogin": "may",
  "ImageService.getImage": "may",
  "ImageService.listImageTags": "may",
  "ImageService.listRegistryLogins": "read",
  "ImageService.searchImages": "may",

  "ImportService.importCoolify": { within: "ImportService.importOnePanel" },
  "ImportService.importDokploy": { within: "ImportService.importOnePanel" },
  "ImportService.importOnePanel": "may",
  "ImportService.planStartPanel": { within: "ImportService.importOnePanel" },
  "ImportService.planStopPanel": "may",

  "LogService.listLogSources": "read",
  "LogService.readLogs": "read",
  "LogService.watchLogs": "read",

  "MemberService.createInvite": { within: "MemberService.listMembers" },
  "MemberService.listMembers": "may",
  "MemberService.revokeInvite": { within: "MemberService.listMembers" },

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
  "ServerService.watchSystemServices": "may",
  "ServerService.readSystemLog": { within: "ServerService.watchSystemServices" },
  "ServerService.watchSystemLog": { within: "ServerService.watchSystemServices" },

  "ServiceService.addServicePort": "may",
  "ServiceService.changeServiceRuntime": "may",
  "ServiceService.deployService": "may",
  "ServiceService.listServiceRevisions": "read",
  "ServiceService.listServices": "read",
  "ServiceService.openService": "may",
  "ServiceService.rollBackService": "may",
  // Asked by name before Update is offered, from the app's page and the catalog's (ADR 0080).
  "ServiceService.upgradeService": "may",

  "ServiceUpdateService.checkServiceUpdates": "may",
  "ServiceUpdateService.listServiceUpdates": "unused",
  "ServiceUpdateService.skipServiceUpdate": "may",
  "ServiceUpdateService.snoozeServiceUpdates": "may",

  "SettingsService.checkPanelDomain": "may",
  "SettingsService.createPanelSignInLink": "may",
  "SettingsService.getControlPlane": "may",
  "SettingsService.getSettings": "read",
  // Asked by name before the New apps panel offers Save (ADR 0080).
  "SettingsService.setAutoUpdateDefault": "may",
  "SettingsService.setInstallName": "may",
  "SettingsService.setPanelDomain": "may",
  "SettingsService.setStepUpFloor": "may",
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
  "VariableService.revealServiceValue": "may",
  "VolumeService.getVolume": "read",

  "WebsiteService.createWebsite": "may",
  "WebsiteService.deleteWebsite": "may",
  "WebsiteService.listWebsiteRuntimes": "read",
  "WebsiteService.listWebsites": "read",
  "WebsiteService.previewWebsite": "read",
  "WebsiteService.updateWebsite": "may",
};
