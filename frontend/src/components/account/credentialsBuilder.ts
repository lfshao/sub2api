export function applyInterceptWarmup(
  credentials: Record<string, unknown>,
  enabled: boolean,
  mode: 'create' | 'edit'
): void {
  if (enabled) {
    credentials.intercept_warmup_requests = true
  } else if (mode === 'edit') {
    delete credentials.intercept_warmup_requests
  }
}

export interface ClaudeCLIProxySettings {
  enabled: boolean
  command: string
  webToolsForwardGroupId: number | null
  userID: string
}

export function applyClaudeCLIProxyExtra(
  extra: Record<string, unknown>,
  settings: ClaudeCLIProxySettings,
  mode: 'create' | 'edit'
): void {
  if (!settings.enabled) {
    if (mode === 'edit') {
      delete extra.claude_cli_proxy_enabled
      delete extra.claude_cli_command
      delete extra.claude_cli_web_tools_forward_group_id
      delete extra.claude_cli_userID
    }
    return
  }

  extra.claude_cli_proxy_enabled = true

  const command = settings.command.trim()
  if (command) {
    extra.claude_cli_command = command
  } else if (mode === 'edit') {
    delete extra.claude_cli_command
  }

  if (settings.webToolsForwardGroupId != null && settings.webToolsForwardGroupId > 0) {
    extra.claude_cli_web_tools_forward_group_id = settings.webToolsForwardGroupId
  } else if (mode === 'edit') {
    delete extra.claude_cli_web_tools_forward_group_id
  }

  const userID = settings.userID.trim()
  if (userID) {
    extra.claude_cli_userID = userID
  } else if (mode === 'edit') {
    delete extra.claude_cli_userID
  }
}

export function applyCLIHiddenForwardingExtra(
  extra: Record<string, unknown>,
  cliProxyEnabled: boolean,
  _mode: 'create' | 'edit'
): void {
  if (!cliProxyEnabled) {
    return
  }

  delete extra.enable_tls_fingerprint
  delete extra.tls_fingerprint_profile_id
  delete extra.session_id_masking_enabled
  delete extra.cache_ttl_override_enabled
  delete extra.cache_ttl_override_target
}

export function applyCustomBaseUrlExtra(
  extra: Record<string, unknown>,
  enabled: boolean,
  value: string,
  mode: 'create' | 'edit',
  allowEmptyEnabled = false
): void {
  const url = value.trim()
  if (enabled && (url || allowEmptyEnabled)) {
    extra.custom_base_url_enabled = true
    if (url) {
      extra.custom_base_url = url
    } else if (mode === 'edit') {
      delete extra.custom_base_url
    }
    return
  }

  if (mode === 'edit') {
    delete extra.custom_base_url_enabled
    delete extra.custom_base_url
  }
}
