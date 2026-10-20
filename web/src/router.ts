import { createRouter, createWebHistory } from 'vue-router'

const routes = [
  { path: '/', name: 'library', component: () => import('./views/LibraryView.vue'), meta: { title: '影视库' } },
  { path: '/v/:id', name: 'detail', component: () => import('./views/DetailView.vue'), meta: { title: '详情' } },
  { path: '/local', name: 'local', component: () => import('./views/LocalView.vue'), meta: { title: '本地' } },
  { path: '/downloads', name: 'downloads', component: () => import('./views/DownloadsView.vue'), meta: { title: '下载' } },
  { path: '/history', name: 'history', component: () => import('./views/HistoryView.vue'), meta: { title: '观看历史' } },
  { path: '/watch/:id', name: 'watch', component: () => import('./views/PlayerView.vue'), meta: { title: '播放' } },
  { path: '/online/:vid', name: 'online', component: () => import('./views/OnlineView.vue'), meta: { title: '在线播放' } },
  // 分享页：给收到链接的人打开，隐藏站点导航（meta.bare）
  { path: '/s/:id', name: 'share', component: () => import('./views/ShareView.vue'), meta: { title: '分享', bare: true } },
  { path: '/favorites', name: 'favorites', component: () => import('./views/FavoritesView.vue'), meta: { title: '收藏' } },
  { path: '/sources', name: 'sources', component: () => import('./views/SourcesView.vue'), meta: { title: '接口' } },
  { path: '/info', name: 'info', component: () => import('./views/InfoView.vue'), meta: { title: '网站信息' } },
  { path: '/:pathMatch(.*)*', redirect: '/' },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
  scrollBehavior(_to, _from, saved) {
    return saved || { top: 0 }
  },
})
