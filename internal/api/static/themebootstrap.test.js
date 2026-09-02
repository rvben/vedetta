const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

// Every page that offers the light theme applies it from a small inline script
// in the head. That script has to run before the stylesheet is linked. Setting
// data-theme after the first style resolution leaves WebKit handing the body
// the dark value of an inherited custom property while the root already reports
// the light one, so the page paints dark text on a dark canvas until some
// unrelated recalculation clears it.
function pagesWithThemeBootstrap() {
  return fs.readdirSync(__dirname)
    .filter(name => name.endsWith('.html'))
    .map(name => ({ name, source: fs.readFileSync(path.join(__dirname, name), 'utf8') }))
    .filter(page => page.source.includes("localStorage.getItem('vedetta-theme')"));
}

test('every themed page applies the theme before it links a stylesheet', () => {
  const pages = pagesWithThemeBootstrap();
  assert.ok(pages.length >= 12, `expected the themed pages to be found, saw ${pages.length}`);

  for (const page of pages) {
    const applied = page.source.indexOf("setAttribute('data-theme', 'light')");
    const stylesheet = page.source.indexOf('rel="stylesheet"');
    assert.notEqual(applied, -1, `${page.name} never applies the saved theme`);
    assert.notEqual(stylesheet, -1, `${page.name} links no stylesheet`);
    assert.ok(applied < stylesheet, `${page.name} applies the theme after linking its stylesheet`);
  }
});

// The script reads the meta tag it recolours, so the tag has to be parsed
// already. Moving the bootstrap above the stylesheet must not move it above the
// tag it depends on.
test('every themed page declares theme-color before the script recolours it', () => {
  for (const page of pagesWithThemeBootstrap()) {
    const meta = page.source.indexOf('name="theme-color"');
    const recolour = page.source.indexOf("'#ffffff'");
    assert.notEqual(meta, -1, `${page.name} declares no theme-color`);
    assert.ok(meta < recolour, `${page.name} recolours theme-color before declaring it`);
  }
});
