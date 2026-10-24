async (providedPage) => {
  const context = await providedPage.context().browser().newContext();
  const page = await context.newPage();
  const base = 'http://127.0.0.1:38086';
  const check = (ok, message) => { if (!ok) throw new Error(message); };
  try {
    await page.goto(base+'/login');
    await page.locator('[name="password"]').fill('d06-local-browser-only-password');
    await Promise.all([page.waitForURL(base+'/admin'),page.locator('button').click()]);
    const list = await page.request.get(base+'/api/admin/users');
    const csrf = (await list.json()).csrf;
    const headers = {Origin:base,'X-CSRF-Token':csrf};
    const groupResponse = await page.request.post(base+'/api/admin/groups/user',{headers,data:{name:'d07-'+Date.now(),node_group_ids:[]}});
    check(groupResponse.status()===201,'group setup');
    const group = await groupResponse.json();
    const response = await page.request.post(base+'/api/admin/users',{headers,data:{name:'precision-'+Date.now(),user_group_id:group.group_id,enabled:true,expires_at:'',quota_bytes:'9007199254740993'}});
    check(response.status()===201,'user setup');
    const user = await response.json();
    check(user.quota_bytes==='9007199254740993','API precision');
    const observed=[];
    for(const value of ['9007199254740993','9223372036854775807','0','']){
      await page.goto(base+'/admin/users');
      const article=page.locator(`article[data-user-id="${user.user_id}"]`);
      const field=article.locator('[name="quota_bytes"]');
      await field.fill(value);
      await Promise.all([page.waitForNavigation(),article.getByRole('button',{name:'保存用户',exact:true}).click()]);
      check(await field.inputValue()===value,'form reload lost precision: '+value);
      const state=await (await page.request.get(base+'/api/admin/users')).json();
      const stored=state.users.find(item=>item.user_id===user.user_id);
      check(stored.quota_bytes===(value===''?null:value),'stored JSON changed amount');
      check(stored.credential_uuid===user.credential_uuid,'quota changed credentials');
      observed.push({form:value,json:stored.quota_bytes});
    }
    return {task:'D07',passed:true,browser:'Chromium',checks:{exact_form_api_sqlite:observed,credential_unchanged:true}};
  } finally {await context.close();}
}
