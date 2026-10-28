<template>
  <div class="vue-app" data-ssr="true">
    <h1>Hello {{ name }} 🟢 (Vue)</h1>
    <p>本页由 <strong>webfront</strong>（纯 Go + goja + @vue/compiler-sfc）在服务端渲染。</p>
    <p>传入的 props.name：<code>{{ name }}</code></p>
  </div>
</template>

<script>
export default {
  name: 'App',
  props: {
    name: { type: String, default: 'Stranger' }
  }
}
</script>
