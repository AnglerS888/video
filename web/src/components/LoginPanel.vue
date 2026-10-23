<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { login } from '../auth'

/**
 * 登录卡片：站点设置了访问密码（信息页 → 运行配置）时，
 * App.vue 在没验证过的浏览器上渲染它，替代正常页面。
 * 登录成功后服务端下发 HttpOnly Cookie，auth.authed 变 true → 页面自动切换。
 */
const pwd = ref('')
const err = ref('')
const busy = ref(false)
const input = ref<HTMLInputElement | null>(null)

onMounted(() => input.value?.focus())

async function submit() {
  if (busy.value) return
  const v = pwd.value.trim()
  if (!v) {
    err.value = '请输入访问密码'
    input.value?.focus()
    return
  }
  busy.value = true
  err.value = ''
  const r = await login(v)
  busy.value = false
  if (!r.ok) {
    err.value = r.msg || '访问密码不正确'
    pwd.value = ''
    input.value?.focus()
  }
}
</script>

<template>
  <div class="flex min-h-[60vh] items-center justify-center py-10">
    <div class="card card-pad w-full max-w-sm space-y-4">
      <div class="flex items-center gap-2.5">
        <span class="grid h-9 w-9 place-items-center rounded-xl bg-amber-500 text-slate-950">
          <svg viewBox="0 0 24 24" class="h-5 w-5" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
            <rect x="4.5" y="10.5" width="15" height="10" rx="2" />
            <path d="M8 10.5V7.5a4 4 0 0 1 8 0v3M12 14.5v2" />
          </svg>
        </span>
        <div class="min-w-0">
          <div class="text-[15px] font-semibold">需要访问密码</div>
          <div class="text-[11px] muted">输入密码后才能进入影视库</div>
        </div>
      </div>

      <form class="space-y-3" @submit.prevent="submit">
        <div>
          <label class="mb-1 block text-[11px] muted" for="login-pwd">访问密码</label>
          <input
            id="login-pwd"
            ref="input"
            v-model="pwd"
            type="password"
            class="input w-full"
            autocomplete="current-password"
            placeholder="请输入访问密码"
            :disabled="busy"
          />
        </div>
        <p v-if="err" class="rounded-lg bg-rose-500/10 px-2.5 py-2 text-[11px] text-rose-500">{{ err }}</p>
        <button type="submit" class="btn btn-primary w-full" :disabled="busy">
          {{ busy ? '验证中…' : '进入' }}
        </button>
      </form>

      <p class="text-[11px] muted">
        密码在「信息」页 →「运行配置」→「访问控制」里修改（忘记时可直接编辑数据目录的
        <code>ui.yml</code> 里的 <code>auth.password</code>，保存即生效）。
        脚本或第三方客户端调用接口时，请带 <code>X-Access-Password</code> 请求头或
        <code>?password=</code> 参数。
      </p>
    </div>
  </div>
</template>
