import { describe, it, expect } from 'vitest'
import {
  applyCLIHiddenForwardingExtra,
  applyClaudeCLIProxyExtra,
  applyCustomBaseUrlExtra,
  applyInterceptWarmup
} from '../credentialsBuilder'

describe('applyInterceptWarmup', () => {
  it('create + enabled=true: should set intercept_warmup_requests to true', () => {
    const creds: Record<string, unknown> = { access_token: 'tok' }
    applyInterceptWarmup(creds, true, 'create')
    expect(creds.intercept_warmup_requests).toBe(true)
  })

  it('create + enabled=false: should not add the field', () => {
    const creds: Record<string, unknown> = { access_token: 'tok' }
    applyInterceptWarmup(creds, false, 'create')
    expect('intercept_warmup_requests' in creds).toBe(false)
  })

  it('edit + enabled=true: should set intercept_warmup_requests to true', () => {
    const creds: Record<string, unknown> = { api_key: 'sk' }
    applyInterceptWarmup(creds, true, 'edit')
    expect(creds.intercept_warmup_requests).toBe(true)
  })

  it('edit + enabled=false + field exists: should delete the field', () => {
    const creds: Record<string, unknown> = { api_key: 'sk', intercept_warmup_requests: true }
    applyInterceptWarmup(creds, false, 'edit')
    expect('intercept_warmup_requests' in creds).toBe(false)
  })

  it('edit + enabled=false + field absent: should not throw', () => {
    const creds: Record<string, unknown> = { api_key: 'sk' }
    applyInterceptWarmup(creds, false, 'edit')
    expect('intercept_warmup_requests' in creds).toBe(false)
  })

  it('should not affect other fields', () => {
    const creds: Record<string, unknown> = {
      api_key: 'sk',
      base_url: 'url',
      intercept_warmup_requests: true
    }
    applyInterceptWarmup(creds, false, 'edit')
    expect(creds.api_key).toBe('sk')
    expect(creds.base_url).toBe('url')
    expect('intercept_warmup_requests' in creds).toBe(false)
  })
})

describe('applyClaudeCLIProxyExtra', () => {
  it('create + enabled=true: should set CLI proxy fields', () => {
    const extra: Record<string, unknown> = {}

    applyClaudeCLIProxyExtra(extra, {
      enabled: true,
      command: ' /usr/local/bin/claude ',
      webToolsForwardGroupId: 12,
      userID: ' abc '
    }, 'create')

    expect(extra).toEqual({
      claude_cli_proxy_enabled: true,
      claude_cli_command: '/usr/local/bin/claude',
      claude_cli_web_tools_forward_group_id: 12,
      claude_cli_userID: 'abc'
    })
  })

  it('create + enabled=false: should not add CLI proxy fields', () => {
    const extra: Record<string, unknown> = {}

    applyClaudeCLIProxyExtra(extra, {
      enabled: false,
      command: 'claude',
      webToolsForwardGroupId: 12,
      userID: 'abc'
    }, 'create')

    expect(extra).toEqual({})
  })

  it('edit + enabled=false: should remove existing CLI proxy fields', () => {
    const extra: Record<string, unknown> = {
      claude_cli_proxy_enabled: true,
      claude_cli_command: 'claude',
      claude_cli_web_tools_forward_group_id: 12,
      claude_cli_userID: 'abc',
      window_cost_limit: 50
    }

    applyClaudeCLIProxyExtra(extra, {
      enabled: false,
      command: '',
      webToolsForwardGroupId: null,
      userID: ''
    }, 'edit')

    expect(extra).toEqual({ window_cost_limit: 50 })
  })
})

describe('applyCLIHiddenForwardingExtra', () => {
  it('should remove direct-forwarding fields when CLI proxy is enabled', () => {
    const extra: Record<string, unknown> = {
      enable_tls_fingerprint: true,
      tls_fingerprint_profile_id: 1,
      session_id_masking_enabled: true,
      cache_ttl_override_enabled: true,
      cache_ttl_override_target: '1h',
      claude_cli_proxy_enabled: true
    }

    applyCLIHiddenForwardingExtra(extra, true, 'edit')

    expect(extra).toEqual({ claude_cli_proxy_enabled: true })
  })

  it('should keep direct-forwarding fields when CLI proxy is disabled', () => {
    const extra: Record<string, unknown> = {
      enable_tls_fingerprint: true,
      tls_fingerprint_profile_id: 1,
      session_id_masking_enabled: true,
      cache_ttl_override_enabled: true,
      cache_ttl_override_target: '1h'
    }

    applyCLIHiddenForwardingExtra(extra, false, 'edit')

    expect(extra).toEqual({
      enable_tls_fingerprint: true,
      tls_fingerprint_profile_id: 1,
      session_id_masking_enabled: true,
      cache_ttl_override_enabled: true,
      cache_ttl_override_target: '1h'
    })
  })
})

describe('applyCustomBaseUrlExtra', () => {
  it('enabled + empty URL + allow empty: should keep enabled flag and remove URL in edit mode', () => {
    const extra: Record<string, unknown> = {
      custom_base_url_enabled: true,
      custom_base_url: 'https://old.example.com'
    }

    applyCustomBaseUrlExtra(extra, true, '', 'edit', true)

    expect(extra).toEqual({ custom_base_url_enabled: true })
  })

  it('enabled + empty URL without allow empty: should delete custom base fields in edit mode', () => {
    const extra: Record<string, unknown> = {
      custom_base_url_enabled: true,
      custom_base_url: 'https://old.example.com'
    }

    applyCustomBaseUrlExtra(extra, true, '', 'edit', false)

    expect(extra).toEqual({})
  })
})
