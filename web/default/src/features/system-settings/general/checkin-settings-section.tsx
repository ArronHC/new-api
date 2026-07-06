/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useCallback, useEffect, useState } from 'react'
import { useForm, type Resolver } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardAction,
  CardContent,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { api } from '@/lib/api'
import { formatQuotaWithCurrency } from '@/lib/currency'
import dayjs from '@/lib/dayjs'

import {
  SettingsForm,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useUpdateOption } from '../hooks/use-update-option'
import type {
  AutoCheckinStatusResponse,
  AutoCheckinTriggerResponse,
  ChannelCheckinResult,
} from '../types'

const schema = z.object({
  enabled: z.boolean(),
  autoCheckinEnabled: z.boolean(),
  autoCheckinCron: z.string(),
})

type Values = z.infer<typeof schema>

const AUTO_CHECKIN_STATUS_QUERY_KEY = ['auto-checkin-status'] as const

function formatAutoCheckinTime(timestamp: number | undefined, fallback: string) {
  if (!timestamp) return fallback
  return dayjs.unix(timestamp).format('YYYY-MM-DD HH:mm:ss')
}

function getChannelResultStatus(result: ChannelCheckinResult) {
  if (result.already_checked) {
    return {
      labelKey: 'Already checked',
      variant: 'neutral' as const,
    }
  }
  if (result.success) {
    return {
      labelKey: 'Success',
      variant: 'success' as const,
    }
  }
  return {
    labelKey: 'Failed',
    variant: 'danger' as const,
  }
}

type ChannelInfo = {
  id: number
  name: string
  base_url: string
  status: number
}

type ChannelCheckinCfg = {
  user_id: string
  access_token: string
}

function ChannelCheckinConfigCard() {
  const { t } = useTranslation()
  const [configs, setConfigs] = useState<Record<string, ChannelCheckinCfg>>({})
  const [dirty, setDirty] = useState(false)

  const channelsQuery = useQuery({
    queryKey: ['channels-for-checkin'],
    queryFn: async () => {
      const res = await api.get('/api/channel/')
      return (res.data?.data?.items ?? []) as ChannelInfo[]
    },
  })

  const configQuery = useQuery({
    queryKey: ['checkin-channel-configs'],
    queryFn: async () => {
      const res = await api.get('/api/option/')
      const options = res.data?.data ?? []
      // options is [{key, value}, ...] from the API
      const optionsByKey: Record<string, string> = {}
      for (const opt of options) {
        if (opt?.key) optionsByKey[opt.key] = opt.value ?? ''
      }
      const val = optionsByKey['checkin_channel_configs'] ?? '{}'
      try {
        return JSON.parse(typeof val === 'string' ? val : '{}') as Record<
          string,
          ChannelCheckinCfg
        >
      } catch {
        return {} as Record<string, ChannelCheckinCfg>
      }
    },
  })

  useEffect(() => {
    if (configQuery.data) {
      setConfigs(configQuery.data)
    }
  }, [configQuery.data])

  const saveMutation = useMutation({
    mutationFn: async () => {
      await api.put('/api/option/', {
        key: 'checkin_channel_configs',
        value: JSON.stringify(configs),
      })
    },
    onSuccess: () => {
      toast.success(t('Saved successfully'))
      setDirty(false)
    },
    onError: () => toast.error(t('Request failed')),
  })

  const updateConfig = useCallback(
    (channelId: number, field: keyof ChannelCheckinCfg, value: string) => {
      setConfigs((prev) => ({
        ...prev,
        [String(channelId)]: {
          ...(prev[String(channelId)] ?? { user_id: '', access_token: '' }),
          [field]: value,
        },
      }))
      setDirty(true)
    },
    []
  )

  const channels = (channelsQuery.data ?? []).filter(
    (c) => c.status === 1 && c.base_url
  )

  if (channels.length === 0) return null

  return (
    <Card size='sm'>
      <CardHeader>
        <CardTitle>{t('Channel Check-in Credentials')}</CardTitle>
        <CardAction>
          <Button
            type='button'
            size='sm'
            variant='outline'
            disabled={!dirty || saveMutation.isPending}
            onClick={() => saveMutation.mutate()}
          >
            {saveMutation.isPending ? t('Saving') : t('Save credentials')}
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent>
        <p className='text-muted-foreground mb-3 text-xs'>
          {t(
            'Configure upstream access credentials for each channel. User ID and access token are needed for the check-in API.'
          )}
        </p>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t('Channel')}</TableHead>
              <TableHead>{t('User ID')}</TableHead>
              <TableHead>{t('Access Token')}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {channels.map((ch) => {
              const cfg = configs[String(ch.id)] ?? { user_id: '', access_token: '' }
              return (
                <TableRow key={ch.id}>
                  <TableCell>
                    <div className='space-y-1'>
                      <p className='font-medium'>{ch.name}</p>
                      <p className='text-muted-foreground text-xs'>
                        {ch.base_url}
                      </p>
                    </div>
                  </TableCell>
                  <TableCell>
                    <Input
                      placeholder='user id'
                      value={cfg.user_id}
                      onChange={(e) =>
                        updateConfig(ch.id, 'user_id', e.target.value)
                      }
                      className='font-mono text-xs'
                    />
                  </TableCell>
                  <TableCell>
                    <Input
                      placeholder='access token'
                      value={cfg.access_token}
                      onChange={(e) =>
                        updateConfig(ch.id, 'access_token', e.target.value)
                      }
                      className='font-mono text-xs'
                    />
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  )
}

export function CheckinSettingsSection({
  defaultValues,
}: {
  defaultValues: {
    enabled: boolean
    autoCheckinEnabled: boolean
    autoCheckinCron: string
  }
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const updateOption = useUpdateOption()

  const form = useForm<Values>({
    resolver: zodResolver(schema) as unknown as Resolver<Values>,
    defaultValues: {
      enabled: defaultValues.enabled,
      autoCheckinEnabled: defaultValues.autoCheckinEnabled,
      autoCheckinCron: defaultValues.autoCheckinCron || '0 0 * * *',
    },
  })

  const { isDirty, isSubmitting } = form.formState
  const enabled = form.watch('enabled')
  const autoCheckinEnabled = form.watch('autoCheckinEnabled')

  const statusQuery = useQuery({
    queryKey: AUTO_CHECKIN_STATUS_QUERY_KEY,
    queryFn: async () => {
      const res = await api.get<AutoCheckinStatusResponse>(
        '/api/user/auto-checkin/status'
      )
      return res.data.data
    },
  })

  const triggerMutation = useMutation({
    mutationFn: async () => {
      const res = await api.post<AutoCheckinTriggerResponse>(
        '/api/user/auto-checkin/trigger',
        undefined,
        { skipBusinessError: true }
      )
      if (!res.data.success) {
        throw new Error(res.data.message || t('Request failed'))
      }
      return res.data.data
    },
    onSuccess: () => {
      toast.success(t('Triggered successfully'))
      queryClient.invalidateQueries({
        queryKey: AUTO_CHECKIN_STATUS_QUERY_KEY,
      })
    },
    onError: (error: Error) => {
      toast.error(error.message || t('Request failed'))
    },
  })

  async function onSubmit(values: Values) {
    const updates: Array<{ key: string; value: string }> = []

    if (values.enabled !== defaultValues.enabled) {
      updates.push({
        key: 'checkin_setting.enabled',
        value: String(values.enabled),
      })
    }

    if (values.autoCheckinEnabled !== defaultValues.autoCheckinEnabled) {
      updates.push({
        key: 'checkin_setting.auto_checkin_enabled',
        value: String(values.autoCheckinEnabled),
      })
    }

    if (values.autoCheckinCron !== defaultValues.autoCheckinCron) {
      updates.push({
        key: 'checkin_setting.auto_checkin_cron',
        value: values.autoCheckinCron,
      })
    }

    if (updates.length === 0) {
      toast.info(t('No changes to save'))
      return
    }

    for (const update of updates) {
      await updateOption.mutateAsync(update)
    }

    form.reset(values)
  }

  return (
    <SettingsSection title={t('Check-in Settings')}>
      <Form {...form}>
        <SettingsForm onSubmit={form.handleSubmit(onSubmit)} autoComplete='off'>
          <SettingsPageFormActions
            onSave={form.handleSubmit(onSubmit)}
            isSaving={updateOption.isPending || isSubmitting}
            isSaveDisabled={!isDirty}
            saveLabel='Save check-in settings'
          />
          <FormField
            control={form.control}
            name='enabled'
            render={({ field }) => (
              <SettingsSwitchItem>
                <SettingsSwitchContent>
                  <FormLabel>{t('Enable check-in feature')}</FormLabel>
                  <FormDescription>
                    {t(
                      'Check in active upstream channels with each channel API key'
                    )}
                  </FormDescription>
                </SettingsSwitchContent>
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={field.onChange}
                    disabled={updateOption.isPending || isSubmitting}
                  />
                </FormControl>
              </SettingsSwitchItem>
            )}
          />

          {enabled && (
            <>
              <FormField
                control={form.control}
                name='autoCheckinEnabled'
                render={({ field }) => (
                  <SettingsSwitchItem>
                    <SettingsSwitchContent>
                      <FormLabel>{t('Enable auto check-in')}</FormLabel>
                    </SettingsSwitchContent>
                    <FormControl>
                      <Switch
                        checked={field.value}
                        onCheckedChange={field.onChange}
                        disabled={updateOption.isPending || isSubmitting}
                      />
                    </FormControl>
                  </SettingsSwitchItem>
                )}
              />

              {autoCheckinEnabled && (
                <>
                  <FormField
                    control={form.control}
                    name='autoCheckinCron'
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>
                          {t('Auto check-in cron expression')}
                        </FormLabel>
                        <FormControl>
                          <Input
                            placeholder='0 0 * * *'
                            disabled={updateOption.isPending || isSubmitting}
                            {...field}
                          />
                        </FormControl>
                        <FormDescription>
                          {t(
                            'Cron expression for auto check-in schedule (minute hour * * *)'
                          )}
                        </FormDescription>
                        <FormMessage />
                      </FormItem>
                    )}
                  />

                  <Card size='sm'>
                    <CardHeader>
                      <CardTitle>{t('Auto check-in status')}</CardTitle>
                      <CardAction>
                        <Button
                          type='button'
                          size='sm'
                          variant='outline'
                          disabled={triggerMutation.isPending}
                          onClick={() => triggerMutation.mutate()}
                        >
                          {triggerMutation.isPending
                            ? t('Running')
                            : t('Trigger Auto Check-in')}
                        </Button>
                      </CardAction>
                    </CardHeader>
                    <CardContent className='space-y-4'>
                      {statusQuery.isLoading ? (
                        <p className='text-muted-foreground text-sm'>
                          {t('Loading')}
                        </p>
                      ) : (
                        <>
                          <div className='grid gap-3 sm:grid-cols-2'>
                            <div className='space-y-1'>
                              <p className='text-muted-foreground text-xs'>
                                {t('Status')}
                              </p>
                              <div className='flex flex-wrap gap-2'>
                                <StatusBadge
                                  label={t(
                                    statusQuery.data?.enabled
                                      ? 'Enabled'
                                      : 'Disabled'
                                  )}
                                  variant={
                                    statusQuery.data?.enabled
                                      ? 'success'
                                      : 'neutral'
                                  }
                                  copyable={false}
                                />
                                <StatusBadge
                                  label={t(
                                    statusQuery.data?.running
                                      ? 'Running'
                                      : 'Idle'
                                  )}
                                  variant={
                                    statusQuery.data?.running
                                      ? 'warning'
                                      : 'neutral'
                                  }
                                  copyable={false}
                                />
                              </div>
                            </div>

                            <div className='space-y-1'>
                              <p className='text-muted-foreground text-xs'>
                                {t('Last run')}
                              </p>
                              <p className='text-sm'>
                                {formatAutoCheckinTime(
                                  statusQuery.data?.last_run_at,
                                  t('Never')
                                )}
                              </p>
                            </div>

                            <div className='space-y-1'>
                              <p className='text-muted-foreground text-xs'>
                                {t('Next run')}
                              </p>
                              <p className='text-sm'>
                                {formatAutoCheckinTime(
                                  statusQuery.data?.next_run_at,
                                  t('Not scheduled')
                                )}
                              </p>
                            </div>
                          </div>

                          {statusQuery.data?.last_summary ? (
                            <div className='space-y-2'>
                              <p className='text-muted-foreground text-xs'>
                                {t('Last result')}
                              </p>
                              <div className='grid gap-3 sm:grid-cols-4'>
                                <div>
                                  <p className='text-muted-foreground text-xs'>
                                    {t('Total channels')}
                                  </p>
                                  <p className='text-sm font-medium'>
                                    {
                                      statusQuery.data.last_summary
                                        .total_channels
                                    }
                                  </p>
                                </div>
                                <div>
                                  <p className='text-muted-foreground text-xs'>
                                    {t('Channels checked in')}
                                  </p>
                                  <p className='text-success text-sm font-medium'>
                                    {
                                      statusQuery.data.last_summary
                                        .channels_checked_in
                                    }
                                  </p>
                                </div>
                                <div>
                                  <p className='text-muted-foreground text-xs'>
                                    {t('Channels already checked')}
                                  </p>
                                  <p className='text-sm font-medium'>
                                    {
                                      statusQuery.data.last_summary
                                        .channels_already_checked
                                    }
                                  </p>
                                </div>
                                <div>
                                  <p className='text-muted-foreground text-xs'>
                                    {t('Failed')}
                                  </p>
                                  <p
                                    className={
                                      statusQuery.data.last_summary
                                        .channels_failed > 0
                                        ? 'text-destructive text-sm font-medium'
                                        : 'text-success text-sm font-medium'
                                    }
                                  >
                                    {
                                      statusQuery.data.last_summary
                                        .channels_failed
                                    }
                                  </p>
                                </div>
                              </div>
                              {statusQuery.data.last_summary.channel_results
                                .length > 0 && (
                                <Table className='min-w-max'>
                                  <TableHeader>
                                    <TableRow>
                                      <TableHead>{t('Channel')}</TableHead>
                                      <TableHead>{t('Status')}</TableHead>
                                      <TableHead>{t('Quota awarded')}</TableHead>
                                      <TableHead>{t('Error')}</TableHead>
                                    </TableRow>
                                  </TableHeader>
                                  <TableBody>
                                    {statusQuery.data.last_summary.channel_results.map(
                                      (result) => {
                                        const resultStatus =
                                          getChannelResultStatus(result)
                                        return (
                                          <TableRow key={result.channel_id}>
                                            <TableCell>
                                              <div className='max-w-64 space-y-1 whitespace-normal'>
                                                <p className='font-medium'>
                                                  {result.channel_name ||
                                                    `#${result.channel_id}`}
                                                </p>
                                                <p className='text-muted-foreground break-all text-xs'>
                                                  {result.base_url}
                                                </p>
                                              </div>
                                            </TableCell>
                                            <TableCell>
                                              <StatusBadge
                                                label={t(
                                                  resultStatus.labelKey
                                                )}
                                                variant={resultStatus.variant}
                                                copyable={false}
                                              />
                                            </TableCell>
                                            <TableCell>
                                              {formatQuotaWithCurrency(
                                                result.quota_awarded
                                              )}
                                            </TableCell>
                                            <TableCell className='max-w-80 whitespace-normal'>
                                              {result.error || '-'}
                                            </TableCell>
                                          </TableRow>
                                        )
                                      }
                                    )}
                                  </TableBody>
                                </Table>
                              )}
                            </div>
                          ) : (
                            <p className='text-muted-foreground text-sm'>
                              {t('No data')}
                            </p>
                          )}

                          {statusQuery.data?.last_error && (
                            <p className='text-destructive text-sm'>
                              {t('Error')}: {statusQuery.data.last_error}
                            </p>
                          )}
                        </>
                      )}
                    </CardContent>
                  </Card>
                </>
              )}

              <ChannelCheckinConfigCard />

            </>
          )}
        </SettingsForm>
      </Form>
    </SettingsSection>
  )
}
