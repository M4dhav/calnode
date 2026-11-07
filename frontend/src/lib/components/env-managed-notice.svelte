<script lang="ts">
	import { t } from '$lib/i18n';

	/**
	 * Says that an environment variable is supplying these settings, so the fields on the
	 * page are the fallback rather than what is in use.
	 *
	 * Without it this page is the new version of the old trap: environment variables now
	 * win, so an admin could edit these fields, see them save, and watch nothing change —
	 * with no more explanation than the old precedence gave in the other direction.
	 *
	 * Plain markup rather than a shadcn component because the kit here has no Alert, and
	 * this reuses the amber notice already established on this settings page and
	 * settings/video. Worth replacing with Alert if one is ever added.
	 */
	let { vars, value = '' }: { vars: string[]; value?: string } = $props();
</script>

<div class="rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-900">
	<p class="font-medium">{$t('settings.envManaged.title')}</p>
	<p class="mt-1 text-amber-800">
		{$t('settings.envManaged.body', { vars: vars.join(', ') })}
	</p>
	{#if value}
		<p class="mt-2 font-mono text-xs text-amber-800">{value}</p>
	{/if}
</div>
