/** @jest-environment node */
// Copyright 2026 The OpenAgent Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/* eslint-env jest */
import TestToolWidget from "./TestToolWidget";

jest.mock("./Editor", () => () => null);
jest.mock("./ChatWidget", () => () => null);
jest.mock("antd", () => ({Select: {}}));
jest.mock("@ant-design/icons", () => ({createFromIconfontCN: () => null}));
jest.mock("casdoor-js-sdk", () => () => null);
jest.mock("../SettingUtil", () => ({}));
jest.mock("../ThemeSetting", () => ({}));

function createWidget(subType, testContent = "") {
  const tool = {type: "web_search", subType, testContent};
  const widget = new TestToolWidget({tool, onUpdateTool: (key, value) => {tool[key] = value;}});
  widget.setState = update => {widget.state = {...widget.state, ...update};};
  widget.state.modelProviders = [{}];
  widget.syncFromTool(tool);
  return {tool, widget};
}

test.each(["", "DuckDuckGo", "Bing", "Google", "Baidu"])("switching the default %s search test to Parallel removes unsupported filters", subType => {
  const {tool, widget} = createWidget(subType);
  expect(JSON.parse(tool.testContent).arguments.language).toBe("en");
  tool.subType = "Parallel";
  widget.componentDidUpdate();
  expect(JSON.parse(tool.testContent)).toEqual({tool: "web_search", arguments: {query: "OpenAgent web search", count: 3}});
});

test.each(["", "DuckDuckGo", "Bing", "Google", "Baidu"])("switching from Parallel restores the %s default test", subType => {
  const {tool, widget} = createWidget("Parallel");
  tool.subType = subType;
  widget.componentDidUpdate();
  expect(JSON.parse(tool.testContent).arguments).toEqual({query: "OpenAgent web search", count: 3, language: "en", country: "us"});
});

test("switching search subtype preserves a user-edited test", () => {
  const custom = JSON.stringify({tool: "web_search", arguments: {query: "my query", count: 2}}, null, 2);
  const {tool, widget} = createWidget("Bing", custom);
  tool.subType = "Parallel";
  widget.componentDidUpdate();
  expect(tool.testContent).toBe(custom);
});
